package consensus

import (
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/consensus/cordial"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// RouterConfig configures a Router.
type RouterConfig struct {
	NodeKey   *ecdsa.PrivateKey
	DB        ethdb.Database
	Sender    common.Sender // requests to other nodes
	Execution Execution
	Publisher Publisher
	Params    cordial.Params // zero for cordial.DefaultParams
}

// Router is the node's Protocol: it runs each contract's chosen protocol.
type Router struct {
	cfg     RouterConfig
	nodeKey ethcommon.Address
	quit    chan struct{}
	wg      sync.WaitGroup

	mu      sync.Mutex
	miners  map[minerKey]*minerEntry
	syncing map[minerKey]bool
	early   map[minerKey][]*cordial.Block // blocks that arrived while syncing
	closed  bool
}

type minerKey struct {
	agent    ethcommon.Address
	contract string
}

type minerEntry struct {
	miner  *cordial.Miner
	outbox *common.Outbox
}

// NewRouter returns a Router. Call Start to load the stored miners.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.Params == (cordial.Params{}) {
		cfg.Params = cordial.DefaultParams
	}
	if cfg.Sender != nil {
		cfg.Sender = &signingSender{inner: cfg.Sender, key: cfg.NodeKey}
	}
	return &Router{
		cfg:     cfg,
		nodeKey: identity.Address(cfg.NodeKey),
		quit:    make(chan struct{}),
		miners:  map[minerKey]*minerEntry{},
		syncing: map[minerKey]bool{},
		early:   map[minerKey][]*cordial.Block{},
	}
}

// NodeKey returns the address of the node's key.
func (r *Router) NodeKey() ethcommon.Address { return r.nodeKey }

// Start loads every stored miner and starts the retry ticker.
func (r *Router) Start() error {
	stored, err := cordial.StoredMiners(r.cfg.DB)
	if err != nil {
		return err
	}
	for _, s := range stored {
		e, err := r.newEntry(s.Agent, s.Contract, func(mc cordial.MinerConfig) (*cordial.Miner, error) {
			return cordial.LoadMiner(mc)
		})
		if err != nil {
			return fmt.Errorf("load miner %s/%s: %w", s.Agent.Hex(), s.Contract, err)
		}
		if err := e.miner.Start(); err != nil {
			return err
		}
	}
	r.wg.Add(1)
	go r.tick()
	return nil
}

func (r *Router) tick() {
	defer r.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.quit:
			return
		case <-t.C:
			r.mu.Lock()
			entries := slices.Collect(maps.Values(r.miners))
			r.mu.Unlock()
			for _, e := range entries {
				e.miner.Tick()
			}
		}
	}
}

// Close stops the ticker and flushes every outbox.
func (r *Router) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.quit)
	entries := slices.Collect(maps.Values(r.miners))
	r.mu.Unlock()
	r.wg.Wait()
	for _, e := range entries {
		e.outbox.Close()
	}
}

// newEntry creates and registers the miner of (agent, contract).
func (r *Router) newEntry(agent ethcommon.Address, contract string, make func(cordial.MinerConfig) (*cordial.Miner, error)) (*minerEntry, error) {
	e := &minerEntry{outbox: common.NewOutbox(r.cfg.Sender)}
	mc := cordial.MinerConfig{
		Agent: agent, NodeKey: r.cfg.NodeKey, Contract: contract, Params: r.cfg.Params,
		Store:    cordial.NewKVStore(r.cfg.DB, agent, contract),
		Network:  &httpNetwork{r: r, agent: agent, outbox: e.outbox},
		Executor: &minerExecution{r: r, agent: agent},
	}
	m, err := make(mc)
	if err != nil {
		e.outbox.Close()
		return nil, err
	}
	e.miner = m
	r.mu.Lock()
	r.miners[minerKey{agent, contract}] = e
	r.mu.Unlock()
	// Execution may have applied records before the miner existed (a
	// newcomer's sync, a restart); tell the miner where it stands.
	if index, root, err := r.cfg.Execution.Progress(agent.Hex(), contract); err == nil && index > 0 {
		if err := m.Applied(index, ethcommon.HexToHash(root)); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func (r *Router) Miner(agent ethcommon.Address, contract string) *cordial.Miner {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.miners[minerKey{agent, contract}]; ok {
		return e.miner
	}
	return nil
}

func parseAgent(agent string) (ethcommon.Address, error) {
	if !ethcommon.IsHexAddress(agent) {
		return ethcommon.Address{}, fmt.Errorf("agent id %q is not an address", agent)
	}
	return ethcommon.HexToAddress(agent), nil
}

// SubmitTx accepts a client's signed transaction.
func (r *Router) SubmitTx(agent string, stx identity.SignedTx) (string, error) {
	id, err := stx.Verify()
	if err != nil {
		return "", err
	}
	addr, err := parseAgent(agent)
	if err != nil {
		return "", err
	}
	if stx.Tx.Agent != addr {
		return "", fmt.Errorf("transaction signed by %s, sent as %s", stx.Tx.Agent.Hex(), addr.Hex())
	}
	if stx.Tx.Kind == identity.KindDeploy {
		return r.deploy(addr, stx, id)
	}
	contract := stx.Tx.Contract
	if e, ok, err := r.cfg.Execution.Guest(addr.Hex(), contract); err != nil {
		return "", err
	} else if ok {
		return r.guestSubmit(addr, contract, e, stx, id)
	}
	if im, ok, err := r.immediate(addr, contract); err != nil {
		return "", err
	} else if ok {
		return r.submitImmediate(im, addr, contract, stx, id)
	}
	m := r.Miner(addr, contract)
	if m == nil {
		if stx.Tx.Kind == identity.KindJoin {
			return "", errors.New("send a join to a member's node (join_contract)")
		}
		return "", fmt.Errorf("%w %s", ErrUnknownContract, contract)
	}
	if _, err := m.SubmitTx(stx); err != nil {
		return "", err
	}
	return id.Hex(), nil
}

// deploy creates a contract with the protocol its deploy names.
func (r *Router) deploy(agent ethcommon.Address, stx identity.SignedTx, id ethcommon.Hash) (string, error) {
	body, _ := stx.Tx.ParseBody()
	db := body.(*identity.DeployBody)
	protocol, err := protocolOf(db.Protocol)
	if err != nil {
		return "", fmt.Errorf("%w %q", err, db.Protocol)
	}
	if db.NodeKey != r.nodeKey {
		return "", fmt.Errorf("deploy names node key %s, this node's key is %s", db.NodeKey.Hex(), r.nodeKey.Hex())
	}
	if exists, err := r.cfg.Execution.AgentExists(agent.Hex()); err != nil {
		return "", err
	} else if !exists {
		return "", fmt.Errorf("%w %q", execution.ErrUnknownAgent, agent.Hex())
	}
	contract := id.Hex()
	if protocol == ProtocolImmediate {
		im := &immediateState{Creator: agent, Next: 1}
		return r.submitImmediate(im, agent, contract, stx, id)
	}
	if m := r.Miner(agent, contract); m != nil {
		return contract, nil // already deployed
	}
	cfg := cordial.NewConfig(cordial.Member{Agent: agent, NodeKey: db.NodeKey, Node: db.Node})
	e, err := r.newEntry(agent, contract, func(mc cordial.MinerConfig) (*cordial.Miner, error) {
		return cordial.NewMiner(mc, cfg, nil, 1)
	})
	if err != nil {
		return "", err
	}
	if _, err := e.miner.SubmitTx(stx); err != nil {
		return "", err
	}
	return contract, nil
}

// RelayJoin sends agent's join transaction to a member's node.
func (r *Router) RelayJoin(ctx context.Context, agent string, stx identity.SignedTx, memberNode, memberAgent string) (string, error) {
	id, err := stx.Verify()
	if err != nil {
		return "", err
	}
	if addr, err := parseAgent(agent); err != nil || stx.Tx.Agent != addr || stx.Tx.Kind != identity.KindJoin {
		return "", errors.New("join_contract needs a join transaction signed by the agent")
	}
	if !ethcommon.IsHexAddress(memberAgent) {
		return "", fmt.Errorf("member agent %q is not an address", memberAgent)
	}
	parsed, err := stx.Tx.ParseBody()
	if err != nil {
		return "", err
	}
	jb := parsed.(*identity.JoinBody)
	if jb.Guest {
		if jb.Host != ethcommon.HexToAddress(memberAgent) {
			return "", errors.New("a guest's join names as its host the member it is sent to")
		}
		// Wait for the host's welcome.
		if err := r.savePending(stx.Tx.Agent, stx.Tx.Contract, pendingJoin{TxID: id.Hex(), Host: jb.Host}); err != nil {
			return "", err
		}
	}
	body, err := r.cfg.Sender.Send(ctx, request(memberNode, ethcommon.HexToAddress(memberAgent), stx.Tx.Contract,
		map[string]any{"type": "tx", "tx": stx}))
	if err == nil {
		if msg := replyMessage(body); msg != "" {
			err = errors.New(msg)
		}
	}
	if err != nil {
		if jb.Guest {
			_ = r.deletePending(stx.Tx.Agent, stx.Tx.Contract)
		}
		return "", err
	}
	return id.Hex(), nil
}

// replyMessage extracts the message of an error reply ({"message": ...}).
func replyMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		return e.Message
	}
	return ""
}

// Messages between nodes, in the "msg" of an a2a_consent request.
type wireMessage struct {
	Type     string             `json:"type"`
	Blocks   []string           `json:"blocks,omitempty"` // hex of encoded blocks
	Hashes   []ethcommon.Hash   `json:"hashes,omitempty"`
	Tx       *identity.SignedTx `json:"tx,omitempty"`
	Config   *cordial.Config    `json:"config,omitempty"`
	Sender   *cordial.Member    `json:"sender,omitempty"`
	Depth    uint64             `json:"depth,omitempty"`
	TxID     string             `json:"txId,omitempty"`
	Approved bool               `json:"approved,omitempty"`
	// A guest's read, the deploy record in a guest's welcome, and a reply
	// a host sends to a guest's node.
	Guest  *ethcommon.Address `json:"guest,omitempty"`
	Method string             `json:"method,omitempty"`
	Values map[string]any     `json:"values,omitempty"`
	Record json.RawMessage    `json:"record,omitempty"`
	Reply  *execution.Reply   `json:"reply,omitempty"`
}

// Receive handles a message from another node: body is the a2a_consent
// request, the message in its envelope signed by the sending node.
func (r *Router) Receive(ctx context.Context, agent, contract string, body []byte) (any, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("a2a_consent: %w", err)
	}
	from, err := env.open(agent, contract)
	if err != nil {
		return nil, err
	}
	var msg wireMessage
	if err := execution.DecodeJSON(env.Msg, &msg); err != nil {
		return nil, fmt.Errorf("a2a_consent: %w", err)
	}
	addr, err := parseAgent(agent)
	if err != nil {
		return nil, err
	}
	// Blocks and transactions carry their own signatures, and a refusal
	// concerns only the joiner; anything else is answered only for the node
	// of a member of the contract's current configuration.
	switch msg.Type {
	case "get_blocks", "get_from", "get_ledger":
		if !r.memberNode(addr, contract, from) {
			return nil, fmt.Errorf("node %s serves no member of contract %s", from.Hex(), contract)
		}
	case "welcome", "guest_welcome":
		if msg.Sender == nil || msg.Sender.NodeKey != from || msg.Config == nil {
			return nil, errors.New("welcome not signed by the welcoming member's node")
		}
		if mb, ok := msg.Config.Member(msg.Sender.Agent); !ok || mb.NodeKey != from {
			return nil, errors.New("welcome from a node that serves no member of the configuration")
		}
	}
	switch msg.Type {
	case "blocks":
		blocks, err := decodeBlocks(msg.Blocks)
		if err != nil {
			return nil, err
		}
		key := minerKey{addr, contract}
		r.mu.Lock()
		e := r.miners[key]
		if r.syncing[key] && (e == nil || !e.miner.Active()) {
			// Members may send blocks of the new configuration before the
			// sync finishes; keep them for the miner.
			r.early[key] = append(r.early[key], blocks...)
			r.mu.Unlock()
			return nil, nil
		}
		r.mu.Unlock()
		if e == nil {
			return nil, nil // not a member here
		}
		return nil, e.miner.ReceiveBlocks(ethcommon.Address{}, blocks)
	case "get_blocks":
		m := r.Miner(addr, contract)
		if m == nil {
			return map[string]any{"blocks": []string{}}, nil
		}
		return map[string]any{"blocks": encodeBlocks(m.Blocks(msg.Hashes))}, nil
	case "get_from":
		m := r.Miner(addr, contract)
		if m == nil {
			return map[string]any{"blocks": []string{}}, nil
		}
		return map[string]any{"blocks": encodeBlocks(m.BlocksFrom(msg.Depth))}, nil
	case "get_ledger":
		// A newcomer's node syncs from a member. Only a node that serves a
		// member of the contract answers.
		if r.Miner(addr, contract) == nil {
			return nil, fmt.Errorf("%w %s", ErrUnknownContract, contract)
		}
		entries, err := r.cfg.Execution.Ledger(addr.Hex(), contract, 0)
		if err != nil {
			return nil, err
		}
		records := make([]json.RawMessage, len(entries))
		for i, e := range entries {
			records[i] = e.Record
		}
		return map[string]any{"records": records}, nil
	case "tx":
		if msg.Tx == nil {
			return nil, errors.New("tx message without a transaction")
		}
		m := r.Miner(addr, contract)
		if m == nil {
			return nil, fmt.Errorf("%w %s", ErrUnknownContract, contract)
		}
		if tx := msg.Tx.Tx; tx.Kind != identity.KindJoin && !m.Config().IsMember(tx.Agent) {
			// A guest's transaction comes from the guest's node.
			if err := r.checkGuest(addr, contract, tx.Agent, from); err != nil {
				return nil, err
			}
		}
		id, err := m.SubmitTx(*msg.Tx)
		if err != nil {
			return nil, err
		}
		return id.Hex(), nil
	case "welcome":
		return nil, r.welcome(addr, contract, msg)
	case "read":
		return r.hostRead(addr, contract, from, msg)
	case "guest_welcome":
		return nil, r.guestWelcome(addr, contract, msg)
	case "event":
		return nil, r.guestEvent(addr, contract, from, msg)
	case "join_result":
		if !msg.Approved {
			_ = r.deletePending(addr, contract)
		}
		r.cfg.Publisher.Publish(agent, execution.Reply{Request: msg.TxID, Contract: &contract,
			Action: "join_contract", Reply: msg.Approved})
		return nil, nil
	}
	return nil, fmt.Errorf("unknown a2a_consent message %q", msg.Type)
}

// memberNode reports whether key is the node key of a member of the current
// configuration of the contract, as agent's miner here knows it.
func (r *Router) memberNode(agent ethcommon.Address, contract string, key ethcommon.Address) bool {
	m := r.Miner(agent, contract)
	if m == nil {
		return false
	}
	for _, mb := range m.Config().Members {
		if mb.NodeKey == key {
			return true
		}
	}
	return false
}

// welcome makes this node a member's node: it syncs the contract's ledger
// from the sender and starts the agent's miner.
func (r *Router) welcome(agent ethcommon.Address, contract string, msg wireMessage) error {
	if msg.Config == nil || msg.Sender == nil {
		return errors.New("welcome without a configuration")
	}
	cfg := msg.Config
	mb, ok := cfg.Member(agent)
	if !ok || mb.NodeKey != r.nodeKey {
		return errors.New("welcome for a member this node does not serve")
	}
	if exists, err := r.cfg.Execution.AgentExists(agent.Hex()); err != nil || !exists {
		return fmt.Errorf("agent %s is not registered here", agent.Hex())
	}
	key := minerKey{agent, contract}
	r.mu.Lock()
	if e, ok := r.miners[key]; (ok && e.miner.Active()) || r.syncing[key] {
		r.mu.Unlock()
		return nil
	}
	r.syncing[key] = true
	r.mu.Unlock()
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.syncing, key)
			r.mu.Unlock()
		}()
		if err := r.sync(agent, contract, cfg, *msg.Sender); err != nil {
			slog.Error("sync from a member failed", "contract", contract, "agent", agent.Hex(), "err", err)
		}
		r.mu.Lock()
		early := r.early[key]
		delete(r.early, key)
		e := r.miners[key]
		r.mu.Unlock()
		if e != nil && len(early) > 0 {
			_ = e.miner.ReceiveBlocks(ethcommon.Address{}, early)
		}
	}()
	return nil
}

// sync fetches the ledger from a member, executes it, and starts the miner.
func (r *Router) sync(agent ethcommon.Address, contract string, cfg *cordial.Config, sender cordial.Member) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	body, err := r.cfg.Sender.Send(ctx, request(sender.Node, sender.Agent, contract, map[string]any{"type": "get_ledger"}))
	if err != nil {
		return err
	}
	var reply struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return fmt.Errorf("ledger reply: %w", err)
	}
	var ids []ethcommon.Hash
	var joinID string
	for i, raw := range reply.Records {
		rec, err := execution.DecodeRecord(raw)
		if err != nil {
			return err
		}
		if rec.Tx == nil {
			return fmt.Errorf("ledger record %d has no transaction", i+1)
		}
		id, err := rec.Tx.Verify()
		if err != nil || id.Hex() != rec.HashCode {
			return fmt.Errorf("ledger record %d: bad signature", i+1)
		}
		rec.Index = uint64(i + 1)
		if err := r.cfg.Execution.Submit(agent.Hex(), rec); err != nil {
			return err
		}
		ids = append(ids, id)
		if rec.Tx.Tx.Kind == identity.KindJoin && rec.Tx.Tx.Agent == agent {
			joinID = rec.HashCode
		}
	}
	key := minerKey{agent, contract}
	r.mu.Lock()
	old := r.miners[key]
	r.mu.Unlock()
	if old != nil {
		old.outbox.Close()
	}
	e, err := r.newEntry(agent, contract, func(mc cordial.MinerConfig) (*cordial.Miner, error) {
		return cordial.NewMiner(mc, cfg, ids, uint64(len(ids))+1)
	})
	if err != nil {
		return err
	}
	slog.Info("joined contract", "contract", contract, "agent", agent.Hex(), "records", len(ids))
	if joinID != "" {
		r.cfg.Publisher.Publish(agent.Hex(), execution.Reply{Request: joinID, Contract: &contract,
			Action: "join_contract", Reply: true})
	}
	return e.miner.Start()
}

// Decided passes execution's decision to the miner, and tells a rejected
// joiner's node about it.
func (r *Router) Decided(agent, contract string, record execution.Record, approved bool) {
	addr, err := parseAgent(agent)
	if err != nil {
		return
	}
	if m := r.Miner(addr, contract); m != nil {
		if err := m.Decided(ethcommon.HexToHash(record.HashCode), approved); err != nil {
			slog.Error("decision", "contract", contract, "err", err)
		}
	}
	if record.Tx == nil || record.Tx.Tx.Kind != identity.KindJoin {
		return
	}
	body, err := record.Tx.Tx.ParseBody()
	if err != nil {
		return
	}
	jb := body.(*identity.JoinBody)
	r.mu.Lock()
	e := r.miners[minerKey{addr, contract}]
	r.mu.Unlock()
	switch {
	case e == nil:
	case approved && jb.Guest:
		r.welcomeGuest(addr, contract, e, record, jb)
	case !approved:
		_ = e.outbox.Put(request(jb.Node, record.Tx.Tx.Agent, contract,
			map[string]any{"type": "join_result", "txId": record.HashCode, "approved": false}))
	}
}

// Applied passes execution's progress to the miner.
func (r *Router) Applied(agent, contract string, index uint64, stateRoot string) {
	addr, err := parseAgent(agent)
	if err != nil {
		return
	}
	if m := r.Miner(addr, contract); m != nil {
		if err := m.Applied(index, ethcommon.HexToHash(stateRoot)); err != nil {
			slog.Error("applied", "contract", contract, "err", err)
		}
	}
}

// request builds an a2a_consent request to an agent's node.
func request(node string, agent ethcommon.Address, contract string, msg any) common.Request {
	return common.Request{
		Method: http.MethodPut,
		URL:    strings.TrimRight(node, "/") + "/ibc/app/" + agent.Hex() + "/" + contract,
		Params: map[string]string{"action": execution.ActionConsent},
		JSON:   map[string]any{"msg": msg},
	}
}

func encodeBlocks(blocks []*cordial.Block) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = hex.EncodeToString(b.Encode())
	}
	return out
}

func decodeBlocks(in []string) ([]*cordial.Block, error) {
	var out []*cordial.Block
	for _, s := range in {
		raw, err := hex.DecodeString(s)
		if err != nil {
			return nil, err
		}
		b, err := cordial.DecodeBlock(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// httpNetwork is a miner's Network: messages go to members' nodes over HTTP.
type httpNetwork struct {
	r      *Router
	agent  ethcommon.Address
	outbox *common.Outbox
}

func (n *httpNetwork) SendBlock(to cordial.Member, contract string, b *cordial.Block) {
	_ = n.outbox.Put(request(to.Node, to.Agent, contract,
		map[string]any{"type": "blocks", "blocks": encodeBlocks([]*cordial.Block{b})}))
}

func (n *httpNetwork) RequestBlocks(from cordial.Member, contract string, hashes []ethcommon.Hash) {
	n.pull(from, contract, map[string]any{"type": "get_blocks", "hashes": hashes})
}

func (n *httpNetwork) RequestFrom(from cordial.Member, contract string, depth uint64) {
	n.pull(from, contract, map[string]any{"type": "get_from", "depth": depth})
}

// pull asks a member for blocks and hands the answer to the miner.
func (n *httpNetwork) pull(from cordial.Member, contract string, msg map[string]any) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		body, err := n.r.cfg.Sender.Send(ctx, request(from.Node, from.Agent, contract, msg))
		if err != nil {
			slog.Warn("pull blocks", "contract", contract, "from", from.Node, "err", err)
			return
		}
		var reply struct {
			Blocks []string `json:"blocks"`
		}
		if err := json.Unmarshal(body, &reply); err != nil {
			return
		}
		blocks, err := decodeBlocks(reply.Blocks)
		if err != nil || len(blocks) == 0 {
			return
		}
		if m := n.r.Miner(n.agent, contract); m != nil {
			_ = m.ReceiveBlocks(from.Agent, blocks)
		}
	}()
}

func (n *httpNetwork) Welcome(to cordial.Member, contract string, cfg *cordial.Config, sender cordial.Member) {
	_ = n.outbox.Put(request(to.Node, to.Agent, contract,
		map[string]any{"type": "welcome", "config": cfg, "sender": sender}))
}

// minerExecution is a miner's Executor: transactions become records for the navigator.
type minerExecution struct {
	r     *Router
	agent ethcommon.Address
}

func (x *minerExecution) Execute(contract string, index uint64, id ethcommon.Hash, stx identity.SignedTx, at *cordial.Block) error {
	rec, err := recordFor(contract, index, id, stx, time.UnixMicro(int64(at.Time)))
	if err != nil {
		return err
	}
	return x.r.cfg.Execution.Submit(x.agent.Hex(), rec)
}

func (x *minerExecution) Decision(contract string, id ethcommon.Hash) (bool, bool, error) {
	return x.r.cfg.Execution.Decision(x.agent.Hex(), contract, id.Hex())
}

// recordFor turns a transaction into the record execution applies.
func recordFor(contract string, index uint64, id ethcommon.Hash, stx identity.SignedTx, at time.Time) (execution.Record, error) {
	tx := stx.Tx
	rec := execution.Record{
		Type:      "TX",
		Action:    execution.ActionForKind[tx.Kind],
		Contract:  contract,
		Agent:     tx.Agent.Hex(),
		HashCode:  id.Hex(),
		Index:     index,
		Timestamp: Timestamp(at),
		Tx:        &stx,
	}
	var fields map[string]any
	if err := execution.DecodeJSON([]byte(tx.Body), &fields); err != nil {
		return rec, err
	}
	switch tx.Kind {
	case identity.KindDeploy:
		fields["pid"] = tx.Agent.Hex()
		fields["address"] = fields["node"]
		rec.Message.Fields = fields
	case identity.KindCall:
		rec.Method, _ = fields["method"].(string)
		rec.Message.Values, _ = fields["values"].(map[string]any)
		if rec.Message.Values == nil {
			rec.Message.Values = map[string]any{}
		}
	default:
		rec.Message.Fields = fields
	}
	return rec, nil
}

// Timestamp formats t as a record timestamp, YYYYMMDDHHMMSSffffff in UTC.
func Timestamp(t time.Time) string {
	return strings.Replace(t.UTC().Format("20060102150405.000000"), ".", "", 1)
}

// immediateState is the order of an "immediate" contract.
type immediateState struct {
	Creator ethcommon.Address `json:"creator"`
	Next    uint64            `json:"next"`
}

func (r *Router) immediateKey(agent ethcommon.Address, contract string) []byte {
	k := append([]byte("ibc/im/"), agent.Bytes()...)
	k = binary.AppendUvarint(k, uint64(len(contract)))
	return append(k, contract...)
}

func (r *Router) immediate(agent ethcommon.Address, contract string) (*immediateState, bool, error) {
	k := r.immediateKey(agent, contract)
	ok, err := r.cfg.DB.Has(k)
	if err != nil || !ok {
		return nil, false, err
	}
	raw, err := r.cfg.DB.Get(k)
	if err != nil {
		return nil, false, err
	}
	var s immediateState
	return &s, true, json.Unmarshal(raw, &s)
}

// submitImmediate orders a transaction of an "immediate" contract at once.
// Only the creator writes to such a contract; it has no partners.
func (r *Router) submitImmediate(s *immediateState, agent ethcommon.Address, contract string, stx identity.SignedTx, id ethcommon.Hash) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stx.Tx.Agent != s.Creator || (stx.Tx.Kind != identity.KindCall && stx.Tx.Kind != identity.KindDeploy) {
		return "", errors.New("an immediate contract takes calls from its creator only")
	}
	rec, err := recordFor(contract, s.Next, id, stx, time.Now())
	if err != nil {
		return "", err
	}
	if err := r.cfg.Execution.Submit(agent.Hex(), rec); err != nil {
		return "", err
	}
	s.Next++
	raw, _ := json.Marshal(s)
	if err := r.cfg.DB.Put(r.immediateKey(agent, contract), raw); err != nil {
		return "", err
	}
	return id.Hex(), nil
}
