package consensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ouripoupko/ibc-go/compiler"
	"github.com/ouripoupko/ibc-go/consensus/cordial"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// Joining a contract, on the joiner's node (docs/consensus.md, §10 and §11).
//
// The agent's join names the hash of the source the agent read. The members
// order it and refuse it if that is not the contract's source. If they accept
// it, each member welcomes the joiner's node with the new configuration and
// where the join left the contract: its ledger index, state root, and the
// hash of the ids of the records up to it. Once
// f+1 members (at least one correct) agree on that point, the joiner's node
// fetches the history, from the member the agent joined through first and
// then from the others, and keeps the first history that is the contract's
// (its first record is the deploy whose id is the contract id) and replays to
// that state root, with those records. It then compiles the deploy's source with the deploy's
// compiler and compares it with the deployed code. On a mismatch it does not
// join: the contract is not what its source says.

// CheckSource compiles a deploy's source with the compiler it names, and
// compares the result with the deploy's code: compiler.Verified,
// compiler.Mismatch, or compiler.Unchecked with the reason.
type CheckSource func(ctx context.Context, source string, compiler json.RawMessage, code string) (check, reason string)

// welcomeNote is one member's welcome of this node.
type welcomeNote struct {
	cfg       *cordial.Config
	sender    cordial.Member
	index     uint64
	stateRoot string
	history   string
}

// point is what a welcome says about the joined contract: its configuration
// and where the change left it.
type point struct {
	cfgIndex  uint64
	anchor    ethcommon.Hash
	index     uint64
	stateRoot string
	history   string
}

func (w welcomeNote) point() point {
	return point{w.cfg.Index, w.cfg.Anchor, w.index, w.stateRoot, w.history}
}

// historyHash is the hash of a history's record ids, in order.
func historyHash(ids []string) string {
	all := make([]byte, 0, 32*len(ids))
	for _, id := range ids {
		all = append(all, ethcommon.HexToHash(id).Bytes()...)
	}
	return crypto.Keccak256Hash(all).Hex()
}

// welcome records a member's welcome of agent's node into a contract, and
// starts the sync once enough members agree.
func (r *Router) welcome(agent ethcommon.Address, contract string, msg wireMessage) error {
	if msg.Config == nil || msg.Sender == nil {
		return errors.New("welcome without a configuration")
	}
	cfg := msg.Config
	mb, ok := cfg.Member(agent)
	if !ok || mb.NodeKey != r.nodeKey {
		return errors.New("welcome for a member this node does not serve")
	}
	if msg.Index == 0 || msg.StateRoot == "" || msg.History == "" {
		return errors.New("welcome without the point the change left the contract at")
	}
	if exists, err := r.cfg.Execution.AgentExists(agent.Hex()); err != nil || !exists {
		return fmt.Errorf("agent %s is not registered here", agent.Hex())
	}
	key := minerKey{agent, contract}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.miners[key]; (ok && e.miner.Active()) || r.syncing[key] {
		return nil
	}
	notes := r.welcomes[key]
	if notes == nil {
		notes = map[ethcommon.Address]welcomeNote{}
		r.welcomes[key] = notes
	}
	notes[msg.Sender.Agent] = welcomeNote{cfg: cfg, sender: *msg.Sender, index: msg.Index, stateRoot: msg.StateRoot,
		history: msg.History}

	// The members before the change: f+1 of them include a correct one.
	n := 0
	for _, m := range cfg.Members {
		if m.Agent != agent {
			n++
		}
	}
	need := (n-1)/3 + 1
	agree := map[point][]welcomeNote{}
	for _, w := range notes {
		agree[w.point()] = append(agree[w.point()], w)
	}
	for _, ws := range agree {
		if len(ws) < need {
			continue
		}
		r.syncing[key] = true
		delete(r.welcomes, key)
		go r.join(agent, contract, ws)
		return nil
	}
	return nil
}

// join syncs agent's node into a contract from the members' welcomes, which
// agree on the point the change left the contract at.
func (r *Router) join(agent ethcommon.Address, contract string, welcomes []welcomeNote) {
	key := minerKey{agent, contract}
	defer func() {
		r.mu.Lock()
		delete(r.syncing, key)
		early := r.early[key]
		delete(r.early, key)
		e := r.miners[key]
		r.mu.Unlock()
		if e != nil && len(early) > 0 {
			_ = e.miner.ReceiveBlocks(ethcommon.Address{}, early)
		}
	}()
	p, err := r.pending(agent, contract)
	if err != nil {
		slog.Error("join", "contract", contract, "err", err)
		return
	}
	at := welcomes[0]
	// The member the agent joined through first, then the others that
	// welcomed this node, then the rest of the configuration.
	var candidates []cordial.Member
	seen := map[ethcommon.Address]bool{agent: true}
	add := func(m cordial.Member) {
		if !seen[m.Agent] {
			seen[m.Agent] = true
			candidates = append(candidates, m)
		}
	}
	if p != nil {
		if m, ok := at.cfg.Member(p.Member); ok {
			add(m)
		}
	}
	for _, w := range welcomes {
		add(w.sender)
	}
	for _, m := range at.cfg.Members {
		add(m)
	}
	joinID := ""
	if p != nil {
		joinID = p.TxID
	}
	for _, from := range candidates {
		records, err := r.history(agent, contract, from, at)
		if err != nil {
			slog.Warn("a member's history of the contract does not check", "contract", contract, "member", from.Agent.Hex(), "err", err)
			continue
		}
		check, reason := r.checkSource(records[0])
		if check == compiler.Mismatch {
			r.retreat(agent, contract, p, joinID, check, reason)
			return
		}
		if err := r.keep(agent, contract, at.cfg, records); err != nil {
			slog.Error("join", "contract", contract, "err", err)
			r.retreat(agent, contract, p, joinID, "", err.Error())
			return
		}
		if err := r.cfg.Execution.SetSourceCheck(agent.Hex(), contract, check, reason); err != nil {
			slog.Error("join: source check", "contract", contract, "err", err)
		}
		if p != nil && p.Publish != nil {
			pb := p.Publish.Publish
			if err := r.cfg.Execution.SetPublic(agent.Hex(), contract, pb.Public, pb.Time); err != nil {
				slog.Warn("join: public", "contract", contract, "err", err)
			}
		}
		if p != nil {
			_ = r.deletePending(agent, contract)
		}
		if joinID == "" {
			joinID = joinOf(records, agent)
		}
		slog.Info("joined contract", "contract", contract, "agent", agent.Hex(), "records", len(records), "source", check)
		if joinID != "" {
			r.cfg.Publisher.Publish(agent.Hex(), execution.Reply{Request: joinID, Contract: &contract,
				Action: "join_contract", Reply: true, SourceCheck: check, Reason: reason, SourceHash: p.sourceHash()})
		}
		return
	}
	r.retreat(agent, contract, p, joinID, "", "no member sent a history that the members' state confirms")
}

// history fetches a contract's history from a member, up to the welcomes'
// point, and checks it: every record signed by its agent; the first the
// deploy whose id is the contract id; the records' ids the welcomes'; and the
// state root after the last the welcomes' state root.
func (r *Router) history(agent ethcommon.Address, contract string, from cordial.Member, at welcomeNote) ([]execution.Record, error) {
	index, stateRoot := at.index, at.stateRoot
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	body, err := r.cfg.Sender.Send(ctx, request(from.Node, from.Agent, contract, map[string]any{"type": "get_ledger"}))
	if err != nil {
		return nil, err
	}
	var reply struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("ledger reply: %w", err)
	}
	if uint64(len(reply.Records)) < index {
		return nil, fmt.Errorf("%d records, the change is at %d", len(reply.Records), index)
	}
	records := make([]execution.Record, index)
	for i, raw := range reply.Records[:index] {
		rec, err := execution.DecodeRecord(raw)
		if err != nil {
			return nil, err
		}
		if rec.Tx == nil {
			return nil, fmt.Errorf("record %d has no transaction", i+1)
		}
		id, err := rec.Tx.Verify()
		if err != nil || id.Hex() != rec.HashCode {
			return nil, fmt.Errorf("record %d: bad signature", i+1)
		}
		rec.Index = uint64(i + 1)
		records[i] = rec
	}
	if records[0].Action != execution.ActionDeployContract || records[0].Tx.Tx.Kind != identity.KindDeploy ||
		records[0].HashCode != contract {
		return nil, errors.New("the first record is not this contract's deploy")
	}
	ids := make([]string, len(records))
	for i, rec := range records {
		ids[i] = rec.HashCode
	}
	if historyHash(ids) != at.history {
		return nil, errors.New("the history's records are not the members'")
	}
	root, err := execution.ReplayRoot(agent.Hex(), contract, records)
	if err != nil {
		return nil, err
	}
	if root != stateRoot {
		return nil, fmt.Errorf("the history replays to %s, the members' state is %s", root, stateRoot)
	}
	return records, nil
}

// checkSource checks a deploy record's source against its code.
func (r *Router) checkSource(deploy execution.Record) (string, string) {
	if r.cfg.CheckSource == nil {
		return compiler.Unchecked, "this node does not check sources"
	}
	fields := deploy.Message.Fields
	source, _ := fields[execution.KeySource].(string)
	code, _ := fields["code"].(string)
	info, err := json.Marshal(fields["compiler"])
	if err != nil {
		return compiler.Mismatch, "the deploy's compiler cannot be read"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return r.cfg.CheckSource(ctx, source, info, code)
}

// keep stores a checked history in the agent's ledger, which execution runs,
// and starts the agent's miner after it.
func (r *Router) keep(agent ethcommon.Address, contract string, cfg *cordial.Config, records []execution.Record) error {
	ids := make([]ethcommon.Hash, len(records))
	for i, rec := range records {
		if err := r.cfg.Execution.Submit(agent.Hex(), rec); err != nil {
			return err
		}
		ids[i] = ethcommon.HexToHash(rec.HashCode)
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
	return e.miner.Start()
}

// retreat gives up a join on the joiner's node: nothing of the contract is
// kept, and the agent hears why.
func (r *Router) retreat(agent ethcommon.Address, contract string, p *pendingJoin, joinID, check, reason string) {
	slog.Warn("did not join contract", "contract", contract, "agent", agent.Hex(), "reason", reason)
	_ = r.deletePending(agent, contract)
	if joinID != "" {
		r.cfg.Publisher.Publish(agent.Hex(), execution.Reply{Request: joinID, Contract: &contract,
			Action: "join_contract", Reply: false, SourceCheck: check, Reason: reason, SourceHash: p.sourceHash()})
	}
}

// sourceHash is the source hash the pending join names, if there is one.
func (p *pendingJoin) sourceHash() string {
	if p == nil || p.SourceHash == (ethcommon.Hash{}) {
		return ""
	}
	return p.SourceHash.Hex()
}

// joinOf returns the id of agent's last join in records.
func joinOf(records []execution.Record, agent ethcommon.Address) string {
	for i := len(records) - 1; i >= 0; i-- {
		if tx := records[i].Tx; tx != nil && tx.Tx.Kind == identity.KindJoin && tx.Tx.Agent == agent {
			return records[i].HashCode
		}
	}
	return ""
}

// welcomeNew tells a node new to a contract (a joiner's, or a member's new
// node) that it is part of the configuration, and where the change that made
// it a member left the contract. Execution has applied the change, or is
// just finishing it.
func (n *httpNetwork) welcomeNew(to cordial.Member, contract string, cfg *cordial.Config, sender cordial.Member) {
	for range 100 {
		index, root, ok, err := n.r.cfg.Execution.ChangePoint(n.agent.Hex(), contract, to.Agent.Hex())
		if err != nil {
			slog.Error("welcome", "contract", contract, "err", err)
			return
		}
		if ok {
			entries, err := n.r.cfg.Execution.Ledger(n.agent.Hex(), contract, 0)
			if err != nil || uint64(len(entries)) < index {
				slog.Error("welcome: the ledger", "contract", contract, "err", err)
				return
			}
			ids := make([]string, index)
			for i, e := range entries[:index] {
				rec, err := execution.DecodeRecord(e.Record)
				if err != nil {
					slog.Error("welcome: the ledger", "contract", contract, "err", err)
					return
				}
				ids[i] = rec.HashCode
			}
			_ = n.outbox.Put(request(to.Node, to.Agent, contract, map[string]any{"type": "welcome",
				"config": cfg, "sender": sender, "index": index, "stateRoot": root, "history": historyHash(ids)}))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	slog.Error("welcome: the change is not applied", "contract", contract, "member", to.Agent.Hex())
}
