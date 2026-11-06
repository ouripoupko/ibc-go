package cordial

import (
	"bytes"
	"fmt"
	"slices"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

// node is a block in the blocklace, with what the blocklace derives from it.
type node struct {
	b          *Block
	hash       ethcommon.Hash
	pointers   []*node // nil entry for the anchor
	selfParent *node
	height     int // position in the creator's chain
	// summary: for each creator, the maximal blocks of that creator this
	// block observes (itself included).
	summary map[ethcommon.Address][]*node
}

// Lace is the blocklace of one contract within one configuration. It is
// not safe for concurrent use.
type Lace struct {
	cfg    *Config
	params Params

	nodes     map[ethcommon.Hash]*node
	byDepth   map[uint64][]*node
	tips      map[ethcommon.Address][]*node // maximal blocks per creator
	maxDepth  uint64
	creators  map[uint64]map[ethcommon.Address]bool // per depth
	output    map[ethcommon.Hash]bool
	finals    map[uint64]*node // final leader per depth (cache)
	lastFinal *node
}

// NewLace returns an empty blocklace for a configuration.
func NewLace(cfg *Config, params Params) *Lace {
	return &Lace{
		cfg:      cfg,
		params:   params,
		nodes:    map[ethcommon.Hash]*node{},
		byDepth:  map[uint64][]*node{},
		tips:     map[ethcommon.Address][]*node{},
		creators: map[uint64]map[ethcommon.Address]bool{},
		output:   map[ethcommon.Hash]bool{},
		finals:   map[uint64]*node{},
	}
}

// Config returns the lace's configuration.
func (l *Lace) Config() *Config { return l.cfg }

// Has reports whether a block is in the lace.
func (l *Lace) Has(h ethcommon.Hash) bool { _, ok := l.nodes[h]; return ok }

// Block returns a block of the lace.
func (l *Lace) Block(h ethcommon.Hash) (*Block, bool) {
	n, ok := l.nodes[h]
	if !ok {
		return nil, false
	}
	return n.b, true
}

// Len is the number of blocks.
func (l *Lace) Len() int { return len(l.nodes) }

// Missing returns the pointers of b that are not in the lace (the anchor
// does not count).
func (l *Lace) Missing(b *Block) []ethcommon.Hash {
	var out []ethcommon.Hash
	for _, p := range b.Pointers {
		if p != l.cfg.Anchor || l.cfg.Index == 0 {
			if !l.Has(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// Validate checks the structural rules of §5 (all but the signature and
// the payload, which need keys and transaction checks). It returns
// errMissing if pointers are not in the lace yet.
func (l *Lace) Validate(b *Block) error {
	cfg := l.cfg
	if b.Config != cfg.Index {
		return fmt.Errorf("block of configuration %d, lace is %d", b.Config, cfg.Index)
	}
	if !cfg.IsMember(b.Creator) {
		return fmt.Errorf("creator %s is not a member", b.Creator.Hex())
	}
	first := cfg.FirstDepth()
	switch {
	case b.Depth < first:
		return fmt.Errorf("depth %d is before the configuration's first round %d", b.Depth, first)
	case b.Depth == first:
		if cfg.Index == 0 {
			if len(b.Pointers) != 0 {
				return fmt.Errorf("a first-round block of configuration 0 has no pointers")
			}
		} else if len(b.Pointers) != 1 || b.Pointers[0] != cfg.Anchor {
			return fmt.Errorf("a first-round block must point to the anchor alone")
		}
		return nil
	}
	if m := l.Missing(b); len(m) > 0 {
		return errMissing
	}
	seen := map[ethcommon.Address]bool{}
	previous := map[ethcommon.Address]bool{}
	for _, h := range b.Pointers {
		p, ok := l.nodes[h]
		if !ok {
			return fmt.Errorf("pointer to a block outside the configuration")
		}
		if seen[p.b.Creator] {
			return fmt.Errorf("two pointers to blocks of %s", p.b.Creator.Hex())
		}
		seen[p.b.Creator] = true
		if p.b.Depth >= b.Depth {
			return fmt.Errorf("pointer to a block of depth %d from depth %d", p.b.Depth, b.Depth)
		}
		if p.b.Depth == b.Depth-1 {
			previous[p.b.Creator] = true
		}
		if p.b.Time > b.Time {
			return fmt.Errorf("block time before a pointed block's time")
		}
	}
	if !cfg.IsSupermajority(len(previous)) {
		return fmt.Errorf("not cordial: %d creators of round %d", len(previous), b.Depth-1)
	}
	return nil
}

// Add adds a validated block. Adding a block twice is a no-op.
func (l *Lace) Add(b *Block) error {
	h := b.Hash()
	if l.Has(h) {
		return nil
	}
	if err := l.Validate(b); err != nil {
		return err
	}
	n := &node{b: b, hash: h, summary: map[ethcommon.Address][]*node{}}
	for _, ph := range b.Pointers {
		p := l.nodes[ph] // nil for the anchor
		n.pointers = append(n.pointers, p)
		if p == nil {
			continue
		}
		if p.b.Creator == b.Creator {
			n.selfParent = p
		}
		for c, ms := range p.summary {
			n.summary[c] = append(n.summary[c], ms...)
		}
	}
	if n.selfParent != nil {
		n.height = n.selfParent.height + 1
	}
	n.summary[b.Creator] = append(n.summary[b.Creator], n)
	for c, ms := range n.summary {
		n.summary[c] = maximal(ms)
	}
	l.nodes[h] = n
	l.byDepth[b.Depth] = append(l.byDepth[b.Depth], n)
	slices.SortFunc(l.byDepth[b.Depth], func(a, c *node) int { return bytes.Compare(a.hash[:], c.hash[:]) })
	if b.Depth > l.maxDepth {
		l.maxDepth = b.Depth
	}
	if l.creators[b.Depth] == nil {
		l.creators[b.Depth] = map[ethcommon.Address]bool{}
	}
	l.creators[b.Depth][b.Creator] = true
	l.tips[b.Creator] = maximal(append(l.tips[b.Creator], n))
	return nil
}

// ancestorOrEqual reports whether a is d or an ancestor of d along the
// creator's chain. Both must be blocks of the same creator.
func ancestorOrEqual(a, d *node) bool {
	for d != nil && d.height > a.height {
		d = d.selfParent
	}
	return d == a
}

// maximal keeps the blocks no other block of the list descends from, without duplicates.
func maximal(ms []*node) []*node {
	var out []*node
	for i, m := range ms {
		dominated := false
		for j, o := range ms {
			if i == j {
				continue
			}
			if o == m {
				if j < i {
					dominated = true // duplicate: keep the first
				}
				continue
			}
			if ancestorOrEqual(m, o) {
				dominated = true
			}
			if dominated {
				break
			}
		}
		if !dominated {
			out = append(out, m)
		}
	}
	return out
}

// observes reports whether b observes x (b ⪰ x).
func observes(b, x *node) bool {
	for _, m := range b.summary[x.b.Creator] {
		if ancestorOrEqual(x, m) {
			return true
		}
	}
	return false
}

// approves reports whether b observes x and no equivocation of x.
func approves(b, x *node) bool {
	ms := b.summary[x.b.Creator]
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !ancestorOrEqual(x, m) {
			return false
		}
	}
	return true
}

// ratifies reports whether head's closure has blocks approving key from a supermajority.
func (l *Lace) ratifies(head, key *node) bool {
	approvers := map[ethcommon.Address]bool{}
	for d := key.b.Depth; d <= head.b.Depth; d++ {
		for _, x := range l.byDepth[d] {
			if approvers[x.b.Creator] {
				continue
			}
			if observes(head, x) && approves(x, key) {
				approvers[x.b.Creator] = true
			}
		}
	}
	return l.cfg.IsSupermajority(len(approvers))
}

// isFinal reports whether a leader block is super-ratified within the finality window.
func (l *Lace) isFinal(leader *node) bool {
	ratifiers := map[ethcommon.Address]bool{}
	last := leader.b.Depth + uint64(l.params.FinalityWindow)
	for d := leader.b.Depth + 1; d <= last; d++ {
		for _, x := range l.byDepth[d] {
			if !ratifiers[x.b.Creator] && l.ratifies(x, leader) {
				ratifiers[x.b.Creator] = true
			}
		}
	}
	return l.cfg.IsSupermajority(len(ratifiers))
}

// leaderBlocks returns the blocks of the leader of the wave starting at depth.
func (l *Lace) leaderBlocks(depth uint64) []*node {
	leader, ok := l.cfg.Leader(depth, l.params.Wavelength)
	if !ok {
		return nil
	}
	var out []*node
	for _, x := range l.byDepth[depth] {
		if x.b.Creator == leader {
			out = append(out, x)
		}
	}
	return out
}

// LastFinalLeader finds the final leader block of the highest depth.
func (l *Lace) LastFinalLeader() *node {
	from := l.cfg.FirstDepth()
	if l.lastFinal != nil {
		from = l.lastFinal.b.Depth + 1
	}
	for d := l.maxDepth; d >= from && d+1 > 0; d-- {
		if f, ok := l.finals[d]; ok {
			l.lastFinal = f
			return f
		}
		for _, x := range l.leaderBlocks(d) {
			if l.isFinal(x) {
				l.finals[d] = x
				l.lastFinal = x
				return x
			}
		}
		if d == 0 {
			break
		}
	}
	return l.lastFinal
}

// previousRatifiedLeader returns the leader block of highest depth below
// head that head ratifies, or nil.
func (l *Lace) previousRatifiedLeader(head *node) *node {
	first := l.cfg.FirstDepth()
	w := uint64(l.params.Wavelength)
	if head.b.Depth < first+w {
		return nil
	}
	for d := head.b.Depth - w; ; d -= w {
		for _, x := range l.leaderBlocks(d) {
			if observes(head, x) && l.ratifies(head, x) {
				return x
			}
		}
		if d < first+w {
			return nil
		}
	}
}

// Fragment is the part of τ's output that one leader contributes.
type Fragment struct {
	Leader *Block
	Blocks []*Block
}

// Tau returns the next fragments of τ's output, oldest first, without
// marking them output. Call MarkOutput for each fragment as it is processed.
func (l *Lace) Tau() []Fragment {
	last := l.LastFinalLeader()
	if last == nil || l.output[last.hash] {
		return nil
	}
	var chain []*node
	for x := last; x != nil && !l.output[x.hash]; x = l.previousRatifiedLeader(x) {
		chain = append(chain, x)
	}
	slices.Reverse(chain)
	visited := map[ethcommon.Hash]bool{}
	var out []Fragment
	for _, leader := range chain {
		out = append(out, Fragment{Leader: leader.b, Blocks: l.xsort(leader, visited)})
	}
	return out
}

// xsort lists the blocks of head's closure not output yet, depth-first along
// the pointers in the order the blocks list them (the prototype's x_sort).
func (l *Lace) xsort(head *node, visited map[ethcommon.Hash]bool) []*Block {
	var out []*Block
	var visit func(n *node)
	visit = func(n *node) {
		if n == nil || l.output[n.hash] || visited[n.hash] {
			return
		}
		visited[n.hash] = true
		for _, p := range n.pointers {
			visit(p)
		}
		out = append(out, n.b)
	}
	visit(head)
	return out
}

// MarkOutput records a fragment's blocks as output.
func (l *Lace) MarkOutput(f Fragment) {
	for _, b := range f.Blocks {
		l.output[b.Hash()] = true
	}
}

// IsOutput reports whether a block was output.
func (l *Lace) IsOutput(h ethcommon.Hash) bool { return l.output[h] }

// CompletedRound returns the last round such that it and every round before
// it in the configuration are cordial, and false if the first round is not.
func (l *Lace) CompletedRound() (uint64, bool) {
	return l.completedRound(nil)
}

// completedRound is CompletedRound counting only creators not in exclude.
func (l *Lace) completedRound(exclude map[ethcommon.Address]bool) (uint64, bool) {
	cordial := func(d uint64) bool {
		n := 0
		for c := range l.creators[d] {
			if !exclude[c] {
				n++
			}
		}
		return l.cfg.IsSupermajority(n)
	}
	first := l.cfg.FirstDepth()
	if !cordial(first) {
		return 0, false
	}
	r := first
	for cordial(r + 1) {
		r++
	}
	return r, true
}

// CreatableRound is the round a correct miner can build on: the completed
// round counting only members not known to equivocate, since correct
// miners do not point to equivocators.
func (l *Lace) CreatableRound() (uint64, bool) {
	return l.completedRound(l.Equivocators())
}

// Equivocators returns the creators with more than one chain.
func (l *Lace) Equivocators() map[ethcommon.Address]bool {
	out := map[ethcommon.Address]bool{}
	for c, ts := range l.tips {
		if len(ts) > 1 {
			out[c] = true
		}
	}
	return out
}

// LastBlockOf returns the creator's latest block, if it has a single chain.
func (l *Lace) LastBlockOf(creator ethcommon.Address) *Block {
	ts := l.tips[creator]
	if len(ts) != 1 {
		return nil
	}
	return ts[0].b
}

// PointersFor chooses the pointers of me's next block at depth: each
// non-equivocating member's latest block below depth, me's own included.
func (l *Lace) PointersFor(me ethcommon.Address, depth uint64) []ethcommon.Hash {
	cfg := l.cfg
	if depth == cfg.FirstDepth() {
		if cfg.Index == 0 {
			return nil
		}
		return []ethcommon.Hash{cfg.Anchor}
	}
	var out []ethcommon.Hash
	for _, m := range cfg.Members {
		ts := l.tips[m.Agent]
		if len(ts) != 1 {
			continue // no block, or an equivocator
		}
		x := ts[0]
		for x != nil && x.b.Depth >= depth {
			x = x.selfParent
		}
		if x != nil {
			out = append(out, x.hash)
		}
	}
	return out
}

// Unoutput reports whether the lace holds a block with transactions that τ
// has not output. Blocks of known equivocators do not count: correct miners
// stop pointing to them, so they may never be output, and counting them
// would keep every miner making blocks forever.
func (l *Lace) Unoutput() bool {
	for h, n := range l.nodes {
		if len(n.b.Payload) > 0 && !l.output[h] && len(l.tips[n.b.Creator]) <= 1 {
			return true
		}
	}
	return false
}

// AheadAt reports whether a non-equivocating member other than me has a
// block at depth: someone has started that round and needs it completed.
func (l *Lace) AheadAt(me ethcommon.Address, depth uint64) bool {
	for _, n := range l.byDepth[depth] {
		if n.b.Creator != me && len(l.tips[n.b.Creator]) <= 1 {
			return true
		}
	}
	return false
}

// Blocks returns every block, ordered by depth.
func (l *Lace) Blocks() []*Block {
	var out []*Block
	for d := l.cfg.FirstDepth(); d <= l.maxDepth; d++ {
		for _, n := range l.byDepth[d] {
			out = append(out, n.b)
		}
	}
	return out
}
