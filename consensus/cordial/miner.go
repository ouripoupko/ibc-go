package cordial

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/ouripoupko/ibc-go/identity"
)

// Executor receives the contract's order and decides membership changes.
type Executor interface {
	// Execute delivers a transaction at its index (1-based) in the
	// contract's order, with the block that carried it.
	Execute(contract string, index uint64, id ethcommon.Hash, tx identity.SignedTx, at *Block) error
	// Decision reports how a membership change was decided, once executed.
	Decision(contract string, id ethcommon.Hash) (approved, known bool, err error)
}

// Network carries messages to other members' miners.
type Network interface {
	// SendBlock pushes a block to a member.
	SendBlock(to Member, contract string, b *Block)
	// RequestBlocks asks a member for blocks by hash. The answer arrives
	// through Miner.ReceiveBlocks.
	RequestBlocks(from Member, contract string, hashes []ethcommon.Hash)
	// RequestFrom asks a member for all its blocks from a depth on, to catch
	// up after lost messages. The answer arrives through Miner.ReceiveBlocks.
	RequestFrom(from Member, contract string, depth uint64)
	// Welcome tells a new member that it is part of a configuration.
	Welcome(to Member, contract string, cfg *Config, sender Member)
}

// ErrInactive is returned by a miner whose agent is no longer a member.
var ErrInactive = errors.New("not a member of the contract")

// change is a membership change τ has output, waiting for execution's decision.
type change struct {
	ID       ethcommon.Hash
	Tx       identity.SignedTx
	Decided  bool
	Approved bool
}

type waitState struct {
	Leader  *Block
	Changes []*change
}

// Miner runs the protocol for one agent in one contract.
type Miner struct {
	mu       sync.Mutex
	me       ethcommon.Address
	nodeKey  *ecdsa.PrivateKey
	contract string
	params   Params
	store    Store
	net      Network
	exec     Executor
	clock    func() uint64

	lace      *Lace
	active    bool
	mempool   map[ethcommon.Hash]identity.SignedTx
	order     []ethcommon.Hash // mempool arrival order
	included  map[ethcommon.Hash]identity.SignedTx
	outputTx  map[ethcommon.Hash]bool
	nextIndex uint64
	waiting   *waitState
	buffer    map[ethcommon.Hash]*Block
	future    map[ethcommon.Hash]*Block
	requested map[ethcommon.Hash]time.Time
	lastRoot  ethcommon.Hash
	applied   uint64    // highest index execution reported applied
	progress  time.Time // last time a block was added or output
}

// MinerConfig gathers a miner's dependencies.
type MinerConfig struct {
	Agent    ethcommon.Address
	NodeKey  *ecdsa.PrivateKey
	Contract string
	Params   Params
	Store    Store
	Network  Network
	Executor Executor
	Clock    func() uint64 // unix microseconds; nil for the system clock
}

// NewMiner creates a miner for a new configuration (a deploy, or a welcome).
// alreadyOutput lists the transactions output before cfg (for a newcomer:
// those of the ledger it synced); nextIndex follows them.
func NewMiner(mc MinerConfig, cfg *Config, alreadyOutput []ethcommon.Hash, nextIndex uint64) (*Miner, error) {
	m := newMiner(mc)
	m.lace = NewLace(cfg, m.params)
	m.active = m.isActive(cfg)
	m.nextIndex = nextIndex
	for _, id := range alreadyOutput {
		m.outputTx[id] = true
	}
	if err := m.store.SaveConfig(cfg); err != nil {
		return nil, err
	}
	if err := m.store.SaveOutput(nil, alreadyOutput, nextIndex); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadMiner restores a miner from its store.
func LoadMiner(mc MinerConfig) (*Miner, error) {
	m := newMiner(mc)
	cfg, ok, err := m.store.LoadConfig()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no configuration stored for contract %s", mc.Contract)
	}
	m.lace = NewLace(cfg, m.params)
	m.active = m.isActive(cfg)
	blocks, err := m.store.Blocks()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(blocks, func(a, b *Block) int { return int(a.Depth) - int(b.Depth) })
	for _, b := range blocks {
		if b.Config == cfg.Index {
			if err := m.lace.Add(b); err != nil {
				slog.Warn("stored block rejected", "contract", m.contract, "err", err)
			}
		}
	}
	outBlocks, outTx, next, err := m.store.OutputState()
	if err != nil {
		return nil, err
	}
	for _, h := range outBlocks {
		m.lace.output[h] = true
	}
	if cfg.Index > 0 {
		m.lace.output[cfg.Anchor] = true
	}
	for _, id := range outTx {
		m.outputTx[id] = true
	}
	m.nextIndex = next
	if m.mempool, m.order, err = m.store.Mempool(); err != nil {
		return nil, err
	}
	if m.included, err = m.store.Included(); err != nil {
		return nil, err
	}
	if m.waiting, err = m.store.LoadWaiting(); err != nil {
		return nil, err
	}
	return m, nil
}

func newMiner(mc MinerConfig) *Miner {
	if mc.Params == (Params{}) {
		mc.Params = DefaultParams
	}
	clock := mc.Clock
	if clock == nil {
		clock = func() uint64 { return uint64(time.Now().UnixMicro()) }
	}
	return &Miner{
		me: mc.Agent, nodeKey: mc.NodeKey, contract: mc.Contract, params: mc.Params,
		store: mc.Store, net: mc.Network, exec: mc.Executor, clock: clock,
		mempool:   map[ethcommon.Hash]identity.SignedTx{},
		included:  map[ethcommon.Hash]identity.SignedTx{},
		outputTx:  map[ethcommon.Hash]bool{},
		buffer:    map[ethcommon.Hash]*Block{},
		future:    map[ethcommon.Hash]*Block{},
		requested: map[ethcommon.Hash]time.Time{},
		progress:  time.Now(),
	}
}

// isActive reports whether this miner acts for a member of cfg: the agent is
// a member, and the configuration holds this node's key for it. After a
// rotate_key, the agent's old node is no longer active.
func (m *Miner) isActive(cfg *Config) bool {
	mb, ok := cfg.Member(m.me)
	return ok && mb.NodeKey == identity.Address(m.nodeKey)
}

// Contract returns the contract id.
func (m *Miner) Contract() string { return m.contract }

// Config returns the current configuration.
func (m *Miner) Config() *Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lace.cfg.Clone()
}

// Active reports whether the agent is a member of the current configuration.
func (m *Miner) Active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// Start resumes work after creation or loading.
func (m *Miner) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.step()
}

// SubmitTx adds a client's transaction to the mempool.
func (m *Miner) SubmitTx(stx identity.SignedTx) (ethcommon.Hash, error) {
	id, err := stx.Verify()
	if err != nil {
		return ethcommon.Hash{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return id, ErrInactive
	}
	if err := m.txAllowed(stx, id, m.lace.cfg); err != nil {
		return id, err
	}
	if _, included := m.included[id]; m.outputTx[id] || m.inMempool(id) || included {
		return id, nil // already known
	}
	if err := m.store.SaveMempool(id, stx); err != nil {
		return id, err
	}
	m.mempool[id] = stx
	m.order = append(m.order, id)
	return id, m.step()
}

func (m *Miner) inMempool(id ethcommon.Hash) bool {
	_, ok := m.mempool[id]
	return ok
}

// txAllowed applies the payload rules of §5 that do not depend on the block.
// Calls and a guest's own leave may come from agents that are not members:
// guests, whom execution knows (it refuses calls from anyone else).
func (m *Miner) txAllowed(stx identity.SignedTx, id ethcommon.Hash, cfg *Config) error {
	tx := stx.Tx
	if tx.Kind == identity.KindDeploy {
		if id.Hex() != m.contract {
			return fmt.Errorf("deploy of another contract")
		}
		return nil
	}
	if tx.Contract != m.contract {
		return fmt.Errorf("transaction for contract %s", tx.Contract)
	}
	member := cfg.IsMember(tx.Agent)
	switch tx.Kind {
	case identity.KindJoin:
		if member {
			return fmt.Errorf("%s is already a member", tx.Agent.Hex())
		}
	case identity.KindCall:
	case identity.KindLeave:
		if !member {
			body, err := tx.ParseBody()
			if err != nil || body.(*identity.LeaveBody).Member != tx.Agent {
				return fmt.Errorf("%s is not a member", tx.Agent.Hex())
			}
		}
	default:
		if !member {
			return fmt.Errorf("%s is not a member", tx.Agent.Hex())
		}
	}
	return nil
}

// ConfigChange reports whether a transaction changes the configuration cfg:
// a member's join, a member's leave or removal, or a key rotation. A guest's
// join and leave are ordinary transactions.
func ConfigChange(stx identity.SignedTx, cfg *Config) bool {
	switch stx.Tx.Kind {
	case identity.KindJoin:
		body, err := stx.Tx.ParseBody()
		return err == nil && !body.(*identity.JoinBody).Guest
	case identity.KindLeave:
		body, err := stx.Tx.ParseBody()
		return err == nil && cfg.IsMember(body.(*identity.LeaveBody).Member)
	case identity.KindRotateKey:
		return true
	}
	return false
}

// validPayload checks every transaction of a received block.
func (m *Miner) validPayload(b *Block) error {
	cfg := m.lace.cfg
	for _, stx := range b.Payload {
		id, err := stx.Verify()
		if err != nil {
			return err
		}
		if stx.Tx.Kind == identity.KindDeploy {
			if cfg.Index != 0 || b.Depth != 0 || stx.Tx.Agent != b.Creator {
				return fmt.Errorf("a deploy only in its creator's first block")
			}
		}
		if err := m.txAllowed(stx, id, cfg); err != nil {
			return err
		}
	}
	return nil
}

// ReceiveBlocks handles blocks pushed by, or pulled from, another member.
func (m *Miner) ReceiveBlocks(from ethcommon.Address, blocks []*Block) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range blocks {
		m.accept(b)
	}
	return m.step()
}

func (m *Miner) accept(b *Block) {
	h := b.Hash()
	cfg := m.lace.cfg
	switch {
	case m.lace.Has(h) || m.buffer[h] != nil:
		return
	case b.Config < cfg.Index:
		return // a left-out block of an earlier configuration
	case b.Config > cfg.Index:
		m.future[h] = b
		return
	}
	member, ok := cfg.Member(b.Creator)
	if !ok {
		return
	}
	if signer, err := b.Signer(); err != nil || signer != member.NodeKey {
		slog.Warn("block with a bad signature", "contract", m.contract, "creator", b.Creator.Hex())
		return
	}
	if err := m.validPayload(b); err != nil {
		slog.Warn("block with an invalid payload", "contract", m.contract, "creator", b.Creator.Hex(), "err", err)
		return
	}
	m.buffer[h] = b
}

// step does everything that can be done now: absorb buffered blocks, run τ,
// decide changes, and create blocks.
func (m *Miner) step() error {
	for progress := true; progress; {
		progress = false
		if m.absorb() {
			progress = true
		}
		done, err := m.runTau()
		if err != nil {
			return err
		}
		if done {
			progress = true
		}
		created, err := m.maybeCreate()
		if err != nil {
			return err
		}
		if created {
			progress = true
		}
	}
	return nil
}

// absorb adds buffered blocks whose pointers are all present.
func (m *Miner) absorb() bool {
	added := false
	for again := true; again; {
		again = false
		for h, b := range m.buffer {
			if missing := m.lace.Missing(b); len(missing) > 0 {
				m.request(b, missing)
				continue
			}
			delete(m.buffer, h)
			if err := m.lace.Add(b); err != nil {
				slog.Warn("invalid block", "contract", m.contract, "creator", b.Creator.Hex(), "err", err)
				continue
			}
			if err := m.store.SaveBlock(b); err != nil {
				slog.Error("save block", "err", err)
			}
			delete(m.requested, h)
			for _, stx := range b.Payload {
				if id, err := stx.Tx.Hash(); err == nil && b.Creator != m.me {
					m.dropMempool(id)
				}
			}
			added, again = true, true
		}
	}
	if added {
		m.progress = time.Now()
	}
	return added
}

// request pulls missing blocks, at most once a second per block.
func (m *Miner) request(b *Block, missing []ethcommon.Hash) {
	now := time.Now()
	var ask []ethcommon.Hash
	for _, h := range missing {
		if t, ok := m.requested[h]; ok && now.Sub(t) < time.Second {
			continue
		}
		if m.buffer[h] != nil {
			continue
		}
		m.requested[h] = now
		ask = append(ask, h)
	}
	if len(ask) == 0 {
		return
	}
	if member, ok := m.lace.cfg.Member(b.Creator); ok && b.Creator != m.me {
		m.net.RequestBlocks(member, m.contract, ask)
	}
}

// stallAfter is how long a miner with work waits for progress before it
// asks the members for their recent blocks. (A variable for tests.)
var stallAfter = time.Second

// Tick retries pulls that have not been answered, from every member, and
// catches up a stalled miner: one that has work to do but has neither
// received nor made a block for a while, for example because a block sent
// to it was lost. It asks every member for its blocks from the round it is
// stuck on (the "send what the other needs" of cordial dissemination, as a pull).
func (m *Miner) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.active && now.Sub(m.progress) > stallAfter && m.hasWork() {
		m.progress = now
		// Ask for everything since the last final leader: blocks below the
		// current round may be what finality is waiting for.
		depth := m.lace.cfg.FirstDepth()
		if f := m.lace.lastFinal; f != nil {
			depth = f.b.Depth
		}
		own := m.lace.LastBlockOf(m.me)
		for _, mb := range m.lace.cfg.Members {
			if mb.Agent == m.me {
				continue
			}
			m.net.RequestFrom(mb, m.contract, depth)
			// Members that missed our latest block may be idle, unaware
			// that a round is waiting for them.
			if own != nil {
				m.net.SendBlock(mb, m.contract, own)
			}
		}
	}
	var ask []ethcommon.Hash
	for h, t := range m.requested {
		if now.Sub(t) > 2*time.Second && !m.lace.Has(h) {
			ask = append(ask, h)
			m.requested[h] = now
		}
	}
	if len(ask) == 0 {
		return
	}
	for _, member := range m.lace.cfg.Members {
		if member.Agent != m.me {
			m.net.RequestBlocks(member, m.contract, ask)
		}
	}
}

// hasWork reports whether the miner needs rounds to progress.
func (m *Miner) hasWork() bool {
	return len(m.mempool) > 0 || m.lace.Unoutput() || m.frozen() || len(m.lace.tips[m.me]) == 0
}

// maxCatchUp bounds the blocks sent for one catch-up request.
const maxCatchUp = 1000

// BlocksFrom returns the blocks of depth ≥ depth, for a member catching up.
func (m *Miner) BlocksFrom(depth uint64) []*Block {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Block
	for d := max(depth, m.lace.cfg.FirstDepth()); d <= m.lace.maxDepth && len(out) < maxCatchUp; d++ {
		for _, n := range m.lace.byDepth[d] {
			out = append(out, n.b)
		}
	}
	return out
}

// Blocks returns blocks by hash, for a member's pull request.
func (m *Miner) Blocks(hashes []ethcommon.Hash) []*Block {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Block
	for _, h := range hashes {
		if b, ok := m.lace.Block(h); ok {
			out = append(out, b)
		} else if b, err := m.store.Block(h); err == nil && b != nil {
			out = append(out, b) // e.g. an anchor from an earlier configuration
		}
	}
	return out
}

// runTau outputs the next fragments, stopping at a membership change.
func (m *Miner) runTau() (bool, error) {
	if m.waiting != nil {
		return m.tryDecide()
	}
	frags := m.lace.Tau()
	if len(frags) == 0 {
		return false, nil
	}
	for _, f := range frags {
		var changes []*change
		var txIDs []ethcommon.Hash
		for _, b := range f.Blocks {
			for _, stx := range b.Payload {
				id, err := stx.Tx.Hash()
				if err != nil || m.outputTx[id] {
					continue
				}
				if err := m.exec.Execute(m.contract, m.nextIndex, id, stx, b); err != nil {
					return false, err
				}
				m.outputTx[id] = true
				m.nextIndex++
				txIDs = append(txIDs, id)
				m.dropMempool(id)
				delete(m.included, id)
				if ConfigChange(stx, m.lace.cfg) {
					changes = append(changes, &change{ID: id, Tx: stx})
				}
			}
		}
		m.lace.MarkOutput(f)
		var hashes []ethcommon.Hash
		for _, b := range f.Blocks {
			hashes = append(hashes, b.Hash())
		}
		if err := m.store.SaveOutput(hashes, txIDs, m.nextIndex); err != nil {
			return false, err
		}
		m.progress = time.Now()
		for _, id := range txIDs {
			if err := m.store.DeleteIncluded(id); err != nil {
				return false, err
			}
		}
		if len(changes) > 0 {
			m.waiting = &waitState{Leader: f.Leader, Changes: changes}
			if err := m.store.SaveWaiting(m.waiting); err != nil {
				return false, err
			}
			if _, err := m.tryDecide(); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return true, nil
}

// Decided is called by execution when a membership change has been decided.
func (m *Miner) Decided(id ethcommon.Hash, approved bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiting == nil {
		return nil
	}
	for _, c := range m.waiting.Changes {
		if c.ID == id {
			c.Decided, c.Approved = true, approved
		}
	}
	return m.step()
}

// Applied is called by execution after it applied the record at index.
func (m *Miner) Applied(index uint64, root ethcommon.Hash) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index > m.applied {
		m.applied, m.lastRoot = index, root
	}
	return m.step()
}

// tryDecide finishes a pending change once every decision is known.
func (m *Miner) tryDecide() (bool, error) {
	w := m.waiting
	for _, c := range w.Changes {
		if c.Decided {
			continue
		}
		approved, known, err := m.exec.Decision(m.contract, c.ID)
		if err != nil {
			return false, err
		}
		if !known {
			return false, nil
		}
		c.Decided, c.Approved = true, approved
	}
	m.waiting = nil
	if err := m.store.SaveWaiting(nil); err != nil {
		return false, err
	}
	var accepted []*change
	for _, c := range w.Changes {
		if c.Approved {
			accepted = append(accepted, c)
		}
	}
	if len(accepted) == 0 {
		return true, nil // rejected: unfreeze, same configuration
	}
	return true, m.switchConfig(w.Leader, accepted)
}

// switchConfig starts the next configuration, anchored at leader (§10).
func (m *Miner) switchConfig(leader *Block, accepted []*change) error {
	old := m.lace.cfg
	next := old.Clone()
	next.Index++
	next.Anchor = leader.Hash()
	next.AnchorDepth = leader.Depth
	for _, c := range accepted {
		body, err := c.Tx.Tx.ParseBody()
		if err != nil {
			continue
		}
		switch b := body.(type) {
		case *identity.JoinBody:
			next.Members = append(next.Members, Member{Agent: c.Tx.Tx.Agent, NodeKey: b.NodeKey, Node: b.Node})
		case *identity.LeaveBody:
			next.Members = slices.DeleteFunc(next.Members, func(mb Member) bool { return mb.Agent == b.Member })
		case *identity.RotateKeyBody:
			for i := range next.Members {
				if next.Members[i].Agent == c.Tx.Tx.Agent {
					next.Members[i].NodeKey = b.NodeKey
					next.Members[i].Node = b.Node
				}
			}
		}
	}
	sortMembers(next.Members)
	if err := m.store.SaveConfig(next); err != nil {
		return err
	}
	if err := m.store.SaveBlock(leader); err != nil {
		return err
	}
	m.lace = NewLace(next, m.params)
	m.lace.output[next.Anchor] = true
	m.active = m.isActive(next)
	// Transactions this miner included that were left out return to the mempool.
	for id, stx := range m.included {
		if !m.outputTx[id] && !m.inMempool(id) {
			m.mempool[id] = stx
			m.order = append(m.order, id)
			if err := m.store.SaveMempool(id, stx); err != nil {
				return err
			}
		}
		if err := m.store.DeleteIncluded(id); err != nil {
			return err
		}
	}
	m.included = map[ethcommon.Hash]identity.SignedTx{}
	m.buffer = map[ethcommon.Hash]*Block{}
	slog.Info("configuration switched", "contract", m.contract, "index", next.Index,
		"members", len(next.Members), "anchor", next.Anchor.Hex()[:10])
	// Welcome every node that is new to the contract: new members, and the
	// new nodes of members that rotated their key.
	if self, ok := next.Member(m.me); ok && m.active {
		for _, mb := range next.Members {
			if prev, was := old.Member(mb.Agent); !was || prev.NodeKey != mb.NodeKey {
				m.net.Welcome(mb, m.contract, next, self)
			}
		}
	}
	for h, b := range m.future {
		delete(m.future, h)
		m.accept(b)
	}
	return nil
}

// frozen reports whether the lace holds a membership change τ has not output.
func (m *Miner) frozen() bool {
	for h, n := range m.lace.nodes {
		if m.lace.output[h] {
			continue
		}
		for _, stx := range n.b.Payload {
			if ConfigChange(stx, m.lace.cfg) {
				return true
			}
		}
	}
	return false
}

// maybeCreate creates the miner's next block if the rules allow and something needs it.
func (m *Miner) maybeCreate() (bool, error) {
	if !m.active || m.waiting != nil {
		return false, nil
	}
	cfg := m.lace.cfg
	own := m.lace.LastBlockOf(m.me)
	hasOwn := len(m.lace.tips[m.me]) > 0
	if hasOwn && own == nil {
		return false, nil // own fork: should never happen
	}
	// The first block of a later configuration carries the state root after
	// the anchor, so wait until execution has caught up.
	if !hasOwn && cfg.Index > 0 && m.applied+1 < m.nextIndex {
		return false, nil
	}
	// A correct miner never skips a round: its next block is one round above
	// its last, once that round is cordial among members it does not know to
	// equivocate. Skipping lets miners that know of more equivocators than
	// others find a round without a supermajority, forever.
	depth := cfg.FirstDepth()
	if hasOwn {
		creatable, ok := m.lace.CreatableRound()
		if !ok || creatable < own.Depth {
			return false, nil
		}
		depth = own.Depth + 1
	}
	payload := m.selectPayload()
	// Idle rule, with catch-up: make a block only if there is something to
	// order, or another member has already started this round (it may hold
	// transactions that need the round completed).
	if hasOwn && len(payload) == 0 && !m.lace.Unoutput() && !m.frozen() && !m.lace.AheadAt(m.me, depth) {
		return false, nil
	}
	pointers := m.lace.PointersFor(m.me, depth)
	t := m.clock()
	for _, h := range pointers {
		if b, ok := m.lace.Block(h); ok && b.Time > t {
			t = b.Time
		}
	}
	b := &Block{
		Contract: m.contract, Config: cfg.Index, Creator: m.me, Depth: depth, Time: t,
		Pointers: pointers, Payload: payload, StateRoot: m.lastRoot,
	}
	if err := b.Sign(m.nodeKey); err != nil {
		return false, err
	}
	if err := m.lace.Add(b); err != nil {
		return false, nil // not cordial yet (for example, equivocators excluded)
	}
	// Saved before it is sent: a restart must not make a different block at this depth.
	if err := m.store.SaveBlock(b); err != nil {
		return false, err
	}
	for _, stx := range payload {
		id, _ := stx.Tx.Hash()
		m.included[id] = stx
		if err := m.store.SaveIncluded(id, stx); err != nil {
			return false, err
		}
		m.dropMempool(id)
	}
	for _, mb := range cfg.Members {
		if mb.Agent != m.me {
			m.net.SendBlock(mb, m.contract, b)
		}
	}
	m.progress = time.Now()
	return true, nil
}

// selectPayload picks mempool transactions for the next block, honouring the freeze.
func (m *Miner) selectPayload() []identity.SignedTx {
	if m.frozen() {
		return nil
	}
	var out []identity.SignedTx
	haveChange := false
	for _, id := range m.order {
		stx, ok := m.mempool[id]
		if !ok || m.outputTx[id] {
			continue
		}
		if err := m.txAllowed(stx, id, m.lace.cfg); err != nil {
			continue
		}
		if ConfigChange(stx, m.lace.cfg) {
			if haveChange {
				continue // one change at a time
			}
			haveChange = true
		}
		out = append(out, stx)
	}
	return out
}

func (m *Miner) dropMempool(id ethcommon.Hash) {
	if _, ok := m.mempool[id]; !ok {
		return
	}
	delete(m.mempool, id)
	m.order = slices.DeleteFunc(m.order, func(x ethcommon.Hash) bool { return x == id })
	if err := m.store.DeleteMempool(id); err != nil {
		slog.Error("delete mempool entry", "err", err)
	}
}

// Status describes the miner, for tests and diagnostics.
type Status struct {
	Config    uint64
	Members   int
	Blocks    int
	NextIndex uint64
	Mempool   int
	Waiting   bool
	Active    bool
}

// Status returns the miner's status.
func (m *Miner) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Config: m.lace.cfg.Index, Members: m.lace.cfg.N(), Blocks: m.lace.Len(),
		NextIndex: m.nextIndex, Mempool: len(m.mempool), Waiting: m.waiting != nil, Active: m.active,
	}
}
