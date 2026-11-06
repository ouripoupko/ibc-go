package cordial

import (
	"crypto/ecdsa"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ouripoupko/ibc-go/identity"
)

// The simulator runs miners in one process. Messages wait in a pool and are
// delivered in an order chosen by a seeded random generator, so every run is
// reproducible. Byzantine miners equivocate: each block they make is sent in
// two versions, to different halves of the others.

const simContract = "0xsimcontract"

type simAgent struct {
	root    *ecdsa.PrivateKey
	node    *ecdsa.PrivateKey
	addr    ethcommon.Address
	member  Member
	miner   *Miner
	exec    *simExec
	byz     bool
	seq     int
	nonce   int
	clock   uint64
	pending []ethcommon.Hash // ids submitted by this agent's client
}

type simMsg struct {
	kind   string // block | request | welcome | applied
	from   ethcommon.Address
	to     ethcommon.Address
	block  *Block
	hashes []ethcommon.Hash
	depth  uint64
	cfg    *Config
	ledger []simRecord
}

type simRecord struct {
	id ethcommon.Hash
	tx identity.SignedTx
}

type sim struct {
	t      *testing.T
	rng    *rand.Rand
	params Params
	agents map[ethcommon.Address]*simAgent
	order  []ethcommon.Address
	pool   []simMsg
	deny   map[ethcommon.Address]bool // rejected joiners
	kinds  map[ethcommon.Hash]string  // submitted transactions, for diagnostics
	drop   float64                    // probability that a pushed block is lost
}

// simExec is a miner's execution: it records the order and approves changes.
type simExec struct {
	records  []simRecord
	decision map[ethcommon.Hash]bool
	deny     map[ethcommon.Address]bool
}

func (e *simExec) Execute(_ string, index uint64, id ethcommon.Hash, tx identity.SignedTx, _ *Block) error {
	if index != uint64(len(e.records))+1 {
		return fmt.Errorf("index %d after %d records", index, len(e.records))
	}
	e.records = append(e.records, simRecord{id, tx})
	switch tx.Tx.Kind {
	case identity.KindJoin:
		e.decision[id] = !e.deny[tx.Tx.Agent]
	case identity.KindLeave, identity.KindRotateKey:
		e.decision[id] = true
	}
	return nil
}

func (e *simExec) Decision(_ string, id ethcommon.Hash) (bool, bool, error) {
	d, ok := e.decision[id]
	return d, ok, nil
}

func (e *simExec) root() ethcommon.Hash {
	h := ethcommon.Hash{}
	for _, r := range e.records {
		h = crypto.Keccak256Hash(h[:], r.id[:])
	}
	return h
}

type simNet struct {
	s    *sim
	from ethcommon.Address
}

func (n simNet) SendBlock(to Member, _ string, b *Block) {
	if n.s.drop > 0 && n.s.rng.Float64() < n.s.drop {
		return // lost in transit
	}
	a := n.s.agents[n.from]
	if a.byz {
		n.s.equivocate(a, to, b)
		return
	}
	n.s.pool = append(n.s.pool, simMsg{kind: "block", from: n.from, to: to.Agent, block: b})
}

func (n simNet) RequestBlocks(from Member, _ string, hashes []ethcommon.Hash) {
	n.s.pool = append(n.s.pool, simMsg{kind: "request", from: n.from, to: from.Agent, hashes: hashes})
}

func (n simNet) RequestFrom(from Member, _ string, depth uint64) {
	n.s.pool = append(n.s.pool, simMsg{kind: "from", from: n.from, to: from.Agent, depth: depth})
}

func (n simNet) Welcome(to Member, _ string, cfg *Config, _ Member) {
	sender := n.s.agents[n.from]
	n.s.pool = append(n.s.pool, simMsg{kind: "welcome", from: n.from, to: to.Agent, cfg: cfg.Clone(),
		ledger: slices.Clone(sender.exec.records)})
}

// equivocate sends a twin of b (different time, so a different hash) to
// every second member, and b itself to the others.
func (s *sim) equivocate(a *simAgent, to Member, b *Block) {
	idx := slices.Index(s.order, to.Agent)
	if idx%2 == 0 {
		s.pool = append(s.pool, simMsg{kind: "block", from: a.addr, to: to.Agent, block: b})
		return
	}
	twin := &Block{Contract: b.Contract, Config: b.Config, Creator: b.Creator, Depth: b.Depth,
		Time: b.Time + 1, Pointers: b.Pointers, Payload: nil, StateRoot: b.StateRoot}
	if err := twin.Sign(a.node); err != nil {
		s.t.Fatal(err)
	}
	s.pool = append(s.pool, simMsg{kind: "block", from: a.addr, to: to.Agent, block: twin})
}

// key derives a private key from the simulation's seed, so runs are reproducible.
func (s *sim) key() *ecdsa.PrivateKey {
	for {
		var b [32]byte
		s.rng.Read(b[:])
		if k, err := crypto.ToECDSA(b[:]); err == nil {
			return k
		}
	}
}

func newSim(t *testing.T, seed int64, params Params) *sim {
	return &sim{t: t, rng: rand.New(rand.NewSource(seed)), params: params,
		agents: map[ethcommon.Address]*simAgent{}, deny: map[ethcommon.Address]bool{},
		kinds: map[ethcommon.Hash]string{}}
}

func (s *sim) newAgent(byz bool) *simAgent {
	root, node := s.key(), s.key()
	a := &simAgent{root: root, node: node, addr: identity.Address(root), byz: byz,
		exec: &simExec{decision: map[ethcommon.Hash]bool{}, deny: s.deny}}
	a.member = Member{Agent: a.addr, NodeKey: identity.Address(node), Node: "sim://" + a.addr.Hex()[:8]}
	s.agents[a.addr] = a
	s.order = append(s.order, a.addr)
	slices.SortFunc(s.order, func(x, y ethcommon.Address) int { return x.Cmp(y) })
	return a
}

func (s *sim) startMiner(a *simAgent, cfg *Config, ledger []simRecord) {
	var ids []ethcommon.Hash
	a.exec.records = nil
	for _, r := range ledger {
		ids = append(ids, r.id)
		_ = a.exec.Execute(simContract, uint64(len(a.exec.records))+1, r.id, r.tx, nil)
	}
	m, err := NewMiner(MinerConfig{
		Agent: a.addr, NodeKey: a.node, Contract: simContract, Params: s.params,
		Store:   NewKVStore(rawdb.NewMemoryDatabase(), a.addr, simContract),
		Network: simNet{s, a.addr}, Executor: a.exec,
		Clock: func() uint64 { a.clock++; return a.clock },
	}, cfg, ids, uint64(len(ids))+1)
	if err != nil {
		s.t.Fatal(err)
	}
	a.miner = m
	if err := m.Applied(uint64(len(ids)), a.exec.root()); err != nil {
		s.t.Fatal(err)
	}
}

// fixed starts miners for a configuration with the given agents.
func (s *sim) fixed(agents ...*simAgent) {
	var ms []Member
	for _, a := range agents {
		ms = append(ms, a.member)
	}
	sortMembers(ms)
	cfg := &Config{Members: ms}
	for _, a := range agents {
		s.startMiner(a, cfg, nil)
	}
}

func (s *sim) tx(a *simAgent, kind, body string) identity.SignedTx {
	a.nonce++
	stx, err := identity.Sign(identity.Transaction{Contract: simContract, Kind: kind, Agent: a.addr,
		Nonce: fmt.Sprint(a.nonce), Body: body}, a.root)
	if err != nil {
		s.t.Fatal(err)
	}
	return stx
}

// call submits a call by a to the miner of via.
func (s *sim) call(a, via *simAgent) {
	a.seq++
	stx := s.tx(a, identity.KindCall, fmt.Sprintf(`{"method":"m","values":{"n":%d}}`, a.seq))
	s.submit(a, via, stx)
}

func (s *sim) submit(a, via *simAgent, stx identity.SignedTx) {
	id, err := via.miner.SubmitTx(stx)
	if err != nil {
		s.t.Fatalf("submit: %v", err)
	}
	s.kinds[id] = stx.Tx.Kind + " " + stx.Tx.Body
	if !a.byz {
		a.pending = append(a.pending, id)
	}
}

// run delivers messages in random order until none are left, also
// reporting execution progress to the miners.
func (s *sim) run(maxSteps int) {
	for step := 0; len(s.pool) > 0; step++ {
		if step > maxSteps {
			s.t.Fatalf("no quiescence after %d deliveries", maxSteps)
		}
		i := s.rng.Intn(len(s.pool))
		msg := s.pool[i]
		s.pool = slices.Delete(s.pool, i, i+1)
		s.deliver(msg)
		for _, addr := range s.order {
			a := s.agents[addr]
			if a.miner != nil && uint64(len(a.exec.records)) > a.miner.applied {
				if err := a.miner.Applied(uint64(len(a.exec.records)), a.exec.root()); err != nil {
					s.t.Fatal(err)
				}
			}
		}
	}
}

func (s *sim) deliver(msg simMsg) {
	to := s.agents[msg.to]
	switch msg.kind {
	case "block":
		if to.miner != nil {
			if err := to.miner.ReceiveBlocks(msg.from, []*Block{msg.block}); err != nil {
				s.t.Fatal(err)
			}
		}
	case "request":
		if to.miner != nil {
			for _, b := range to.miner.Blocks(msg.hashes) {
				s.pool = append(s.pool, simMsg{kind: "block", from: msg.to, to: msg.from, block: b})
			}
		}
	case "from":
		if to.miner != nil {
			for _, b := range to.miner.BlocksFrom(msg.depth) {
				s.pool = append(s.pool, simMsg{kind: "block", from: msg.to, to: msg.from, block: b})
			}
		}
	case "welcome":
		// A welcome starts the agent's miner on a node that has none for
		// the contract: a newcomer, or the new node after a rotate_key.
		// The welcome is addressed to the node holding the member's key in
		// the new configuration; start a miner there if it has none.
		mb, _ := msg.cfg.Member(to.addr)
		if to.miner == nil || (identity.Address(to.miner.nodeKey) != mb.NodeKey && mb.NodeKey == identity.Address(to.node)) {
			s.startMiner(to, msg.cfg, msg.ledger)
		}
	}
}

// check verifies that correct miners' outputs are consistent, and (if
// complete) that every correct client's transaction was output.
func (s *sim) check(complete bool) {
	var correct []*simAgent
	for _, addr := range s.order {
		if a := s.agents[addr]; !a.byz && a.miner != nil {
			correct = append(correct, a)
		}
	}
	for i, a := range correct {
		for _, b := range correct[i+1:] {
			n := min(len(a.exec.records), len(b.exec.records))
			for k := 0; k < n; k++ {
				if a.exec.records[k].id != b.exec.records[k].id {
					s.t.Fatalf("outputs of %s and %s differ at index %d", a.addr.Hex()[:8], b.addr.Hex()[:8], k+1)
				}
			}
		}
	}
	if !complete {
		return
	}
	for _, a := range correct {
		if !a.miner.Active() {
			continue
		}
		got := map[ethcommon.Hash]bool{}
		for _, r := range a.exec.records {
			got[r.id] = true
		}
		for _, c := range correct {
			for _, id := range c.pending {
				if !got[id] {
					s.dump()
					s.t.Fatalf("%s did not output a transaction of %s: %s (%d records, status %+v)",
						a.addr.Hex()[:8], c.addr.Hex()[:8], s.kinds[id], len(a.exec.records), a.miner.Status())
				}
			}
		}
	}
}

// dump prints every miner's view, to diagnose a failed run.
func (s *sim) dump() {
	for _, addr := range s.order {
		a := s.agents[addr]
		if a.miner == nil {
			continue
		}
		l := a.miner.lace
		own := l.LastBlockOf(a.addr)
		od := -1
		if own != nil {
			od = int(own.Depth)
		}
		cr, _ := l.CompletedRound()
		cc, _ := l.CreatableRound()
		lf := -1
		if f := l.LastFinalLeader(); f != nil {
			lf = int(f.b.Depth)
		}
		s.t.Logf("%s byz=%v own=%d completed=%d creatable=%d max=%d equivocators=%d lastFinal=%d unoutput=%v frozen=%v records=%d",
			a.addr.Hex()[:8], a.byz, od, cr, cc, l.maxDepth, len(l.Equivocators()), lf, l.Unoutput(), a.miner.frozen(), len(a.exec.records))
		for d := uint64(0); d <= l.maxDepth; d++ {
			line := ""
			for _, n := range l.byDepth[d] {
				mark := ""
				if l.output[n.hash] {
					mark = "*"
				}
				if len(n.b.Payload) > 0 {
					mark += "p"
				}
				line += n.b.Creator.Hex()[2:6] + mark + " "
			}
			ld, isLeader := l.cfg.Leader(d, s.params.Wavelength)
			ls := ""
			if isLeader {
				ls = " leader=" + ld.Hex()[2:6]
			}
			s.t.Logf("   d=%d%s: %s", d, ls, line)
		}
	}
}

var paramVariants = map[string]Params{
	"prototype (w=3, r+3)": DefaultParams,
	"paper (w=3, r+2)":     {Wavelength: 3, FinalityWindow: 2},
	"w=4, r+3":             {Wavelength: 4, FinalityWindow: 3},
}

func TestSingleMember(t *testing.T) {
	s := newSim(t, 1, DefaultParams)
	a := s.newAgent(false)
	s.fixed(a)
	for i := 0; i < 5; i++ {
		s.call(a, a)
		s.run(10_000)
	}
	s.check(true)
	if n := len(a.exec.records); n != 5 {
		t.Fatalf("%d records, want 5", n)
	}
}

func TestCorrectMinersAgree(t *testing.T) {
	for name, params := range paramVariants {
		t.Run(name, func(t *testing.T) {
			for seed := int64(1); seed <= 30; seed++ {
				s := newSim(t, seed, params)
				var as []*simAgent
				for i := 0; i < 4; i++ {
					as = append(as, s.newAgent(false))
				}
				s.fixed(as...)
				for round := 0; round < 4; round++ {
					for _, a := range as {
						s.call(a, a)
					}
					for k := s.rng.Intn(40); k > 0 && len(s.pool) > 0; k-- {
						s.run1()
					}
				}
				s.run(200_000)
				s.check(true)
			}
		})
	}
}

// run1 delivers a single message.
func (s *sim) run1() {
	i := s.rng.Intn(len(s.pool))
	msg := s.pool[i]
	s.pool = slices.Delete(s.pool, i, i+1)
	s.deliver(msg)
}

func TestEquivocatorsAreTolerated(t *testing.T) {
	cases := []struct{ n, byz int }{{4, 1}, {7, 2}}
	for name, params := range paramVariants {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s n=%d f=%d", name, c.n, c.byz), func(t *testing.T) {
				for seed := int64(1); seed <= 25; seed++ {
					s := newSim(t, seed, params)
					var as []*simAgent
					for i := 0; i < c.n; i++ {
						as = append(as, s.newAgent(i < c.byz))
					}
					s.fixed(as...)
					for round := 0; round < 3; round++ {
						for _, a := range as {
							s.call(a, a)
						}
						for k := s.rng.Intn(60); k > 0 && len(s.pool) > 0; k-- {
							s.run1()
						}
						s.check(false)
					}
					s.run(500_000)
					s.check(true)
				}
			})
		}
	}
}

func TestMembershipChanges(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		s := newSim(t, seed, DefaultParams)
		creator := s.newAgent(false)
		s.fixed(creator)
		members := []*simAgent{creator}
		// Three agents join one after the other, with calls in between.
		for j := 0; j < 3; j++ {
			newcomer := s.newAgent(false)
			via := members[s.rng.Intn(len(members))]
			body := fmt.Sprintf(`{"node":%q,"nodeKey":%q}`, newcomer.member.Node, newcomer.member.NodeKey.Hex())
			stx := s.tx(newcomer, identity.KindJoin, body)
			if _, err := via.miner.SubmitTx(stx); err != nil {
				t.Fatal(err)
			}
			for _, m := range members {
				s.call(m, m)
			}
			s.run(500_000)
			if newcomer.miner == nil {
				t.Fatalf("seed %d: newcomer %d was not welcomed", seed, j)
			}
			members = append(members, newcomer)
			for _, m := range members {
				if st := m.miner.Status(); st.Config != uint64(j+1) || st.Members != j+2 {
					t.Fatalf("seed %d: %s at configuration %+v, want %d with %d members", seed, m.addr.Hex()[:8], st, j+1, j+2)
				}
			}
			s.check(true)
		}
		// Everyone writes in the 4-member configuration.
		for _, m := range members {
			s.call(m, m)
		}
		s.run(500_000)
		s.check(true)
		// The first joiner leaves; then a rotate_key.
		leaver := members[1]
		s.submit(leaver, leaver, s.tx(leaver, identity.KindLeave, fmt.Sprintf(`{"member":%q}`, leaver.addr.Hex())))
		s.run(500_000)
		if leaver.miner.Active() {
			t.Fatalf("seed %d: leaver still active", seed)
		}
		// The rotator moves to a new node: its new miner is welcomed and syncs.
		rotator := members[2]
		newNode := s.key()
		s.submit(rotator, rotator, s.tx(rotator, identity.KindRotateKey,
			fmt.Sprintf(`{"node":"sim://new","nodeKey":%q}`, identity.Address(newNode).Hex())))
		oldMiner := rotator.miner
		rotator.node = newNode
		s.run(500_000)
		// The old node may never hear of the switch (members now send to the
		// new node); if it did switch, it must have stopped.
		if st := oldMiner.Status(); st.Config >= 5 && st.Active {
			t.Fatalf("seed %d: the rotator's old node is still active", seed)
		}
		if rotator.miner == oldMiner || !rotator.miner.Active() {
			t.Fatalf("seed %d: the rotator's new node did not take over", seed)
		}
		for _, m := range []*simAgent{members[0], members[2], members[3]} {
			s.call(m, m)
		}
		s.run(500_000)
		s.check(true)
	}
}

func TestRejectedJoinUnfreezes(t *testing.T) {
	s := newSim(t, 7, DefaultParams)
	a, b := s.newAgent(false), s.newAgent(false)
	s.fixed(a, b)
	stranger := s.newAgent(false)
	s.deny[stranger.addr] = true
	stx := s.tx(stranger, identity.KindJoin, fmt.Sprintf(`{"node":"x","nodeKey":%q}`, stranger.member.NodeKey.Hex()))
	if _, err := a.miner.SubmitTx(stx); err != nil {
		t.Fatal(err)
	}
	s.call(a, a)
	s.call(b, b)
	s.run(200_000)
	s.check(true)
	if st := a.miner.Status(); st.Config != 0 || st.Members != 2 {
		t.Fatalf("after a rejected join: %+v", st)
	}
	if stranger.miner != nil {
		t.Fatal("rejected joiner was welcomed")
	}
}

func TestMinerRestart(t *testing.T) {
	s := newSim(t, 3, DefaultParams)
	a, b := s.newAgent(false), s.newAgent(false)
	db := rawdb.NewMemoryDatabase()
	cfg := &Config{Members: []Member{a.member, b.member}}
	sortMembers(cfg.Members)
	mk := func(x *simAgent, load bool) {
		mc := MinerConfig{Agent: x.addr, NodeKey: x.node, Contract: simContract, Params: s.params,
			Store: NewKVStore(db, x.addr, simContract), Network: simNet{s, x.addr}, Executor: x.exec,
			Clock: func() uint64 { x.clock++; return x.clock }}
		var err error
		if load {
			x.miner, err = LoadMiner(mc)
		} else {
			x.miner, err = NewMiner(mc, cfg, nil, 1)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := x.miner.Start(); err != nil {
			t.Fatal(err)
		}
	}
	mk(a, false)
	mk(b, false)
	s.call(a, a)
	s.call(b, b)
	s.run(100_000)
	s.check(true)
	before := len(a.exec.records)

	// a restarts from its store, with messages lost in between.
	mk(a, true)
	if st := a.miner.Status(); st.NextIndex != uint64(before)+1 || st.Blocks == 0 {
		t.Fatalf("after restart: %+v, %d records", st, before)
	}
	s.call(a, a)
	s.call(b, b)
	s.run(100_000)
	s.check(true)
	if len(a.exec.records) != before+2 {
		t.Fatalf("%d records after restart, want %d", len(a.exec.records), before+2)
	}
}

// Lost blocks are recovered: stalled miners catch up by pulling from members.
func TestLostBlocksAreRecovered(t *testing.T) {
	old := stallAfter
	stallAfter = 0
	defer func() { stallAfter = old }()
	for seed := int64(1); seed <= 20; seed++ {
		s := newSim(t, seed, DefaultParams)
		s.drop = 0.3
		var as []*simAgent
		for i := 0; i < 4; i++ {
			as = append(as, s.newAgent(false))
		}
		s.fixed(as...)
		for round := 0; round < 3; round++ {
			for _, a := range as {
				s.call(a, a)
			}
		}
		for i := 0; i < 200; i++ {
			s.run(200_000)
			if s.complete() {
				break
			}
			for _, a := range as {
				a.miner.Tick()
			}
		}
		s.check(true)
	}
}

// complete reports whether every correct miner has output every correct
// client's transactions.
func (s *sim) complete() bool {
	for _, a := range s.agents {
		if a.byz || a.miner == nil {
			continue
		}
		got := map[ethcommon.Hash]bool{}
		for _, r := range a.exec.records {
			got[r.id] = true
		}
		for _, c := range s.agents {
			for _, id := range c.pending {
				if !got[id] {
					return false
				}
			}
		}
	}
	return true
}
