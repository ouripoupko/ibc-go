// Package cordial implements the Cordial Miners consensus protocol
// (eventual synchrony), as designed in docs/consensus.md.
package cordial

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"slices"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ouripoupko/ibc-go/identity"
)

// Block is a block of a contract's blocklace.
type Block struct {
	Contract  string
	Config    uint64
	Creator   ethcommon.Address
	Depth     uint64
	Time      uint64 // unix microseconds
	Pointers  []ethcommon.Hash
	Payload   []identity.SignedTx
	StateRoot ethcommon.Hash
	Signature []byte

	hash ethcommon.Hash // cached
}

// blockContent is what the hash covers: everything but the signature.
type blockContent struct {
	Contract  string
	Config    uint64
	Creator   ethcommon.Address
	Depth     uint64
	Time      uint64
	Pointers  []ethcommon.Hash
	Payload   []identity.SignedTx
	StateRoot ethcommon.Hash
}

func (b *Block) content() blockContent {
	return blockContent{b.Contract, b.Config, b.Creator, b.Depth, b.Time, b.Pointers, b.Payload, b.StateRoot}
}

// Hash returns the keccak256 of the block's RLP encoding without the signature.
func (b *Block) Hash() ethcommon.Hash {
	if b.hash == (ethcommon.Hash{}) {
		enc, err := rlp.EncodeToBytes(b.content())
		if err != nil {
			panic(fmt.Sprintf("encode block: %v", err))
		}
		b.hash = crypto.Keccak256Hash(enc)
	}
	return b.hash
}

// Sign signs the block with the node key.
func (b *Block) Sign(nodeKey *ecdsa.PrivateKey) error {
	b.hash = ethcommon.Hash{}
	h := b.Hash()
	sig, err := crypto.Sign(h[:], nodeKey)
	if err != nil {
		return err
	}
	b.Signature = sig
	return nil
}

// Signer returns the address of the key that signed the block.
func (b *Block) Signer() (ethcommon.Address, error) {
	return identity.Recover(b.Hash(), b.Signature)
}

// Encode returns the block's RLP encoding, signature included.
func (b *Block) Encode() []byte {
	enc, err := rlp.EncodeToBytes(struct {
		C   blockContent
		Sig []byte
	}{b.content(), b.Signature})
	if err != nil {
		panic(fmt.Sprintf("encode block: %v", err))
	}
	return enc
}

// DecodeBlock parses a block encoded with Encode.
func DecodeBlock(data []byte) (*Block, error) {
	var dec struct {
		C   blockContent
		Sig []byte
	}
	if err := rlp.DecodeBytes(data, &dec); err != nil {
		return nil, fmt.Errorf("decode block: %w", err)
	}
	c := dec.C
	return &Block{
		Contract: c.Contract, Config: c.Config, Creator: c.Creator, Depth: c.Depth, Time: c.Time,
		Pointers: c.Pointers, Payload: c.Payload, StateRoot: c.StateRoot, Signature: dec.Sig,
	}, nil
}

// Member is a miner of a configuration.
type Member struct {
	Agent   ethcommon.Address `json:"agent"`
	NodeKey ethcommon.Address `json:"nodeKey"`
	Node    string            `json:"node"` // the URL of the member's node
}

// Config is a contract's membership configuration (§10 of the design).
type Config struct {
	Index       uint64         `json:"index"`
	Members     []Member       `json:"members"` // sorted by agent
	Anchor      ethcommon.Hash `json:"anchor"`  // zero for configuration 0
	AnchorDepth uint64         `json:"anchorDepth"`
}

// NewConfig returns configuration 0 with a single member.
func NewConfig(creator Member) *Config {
	return &Config{Members: []Member{creator}}
}

// FirstDepth is the depth of the configuration's first round.
func (c *Config) FirstDepth() uint64 {
	if c.Index == 0 {
		return 0
	}
	return c.AnchorDepth + 1
}

// N is the number of members.
func (c *Config) N() int { return len(c.Members) }

// F is the number of faults the configuration tolerates.
func (c *Config) F() int { return (len(c.Members) - 1) / 3 }

// IsSupermajority reports whether count members are a supermajority.
func (c *Config) IsSupermajority(count int) bool { return 2*count > c.N()+c.F() }

// Member returns the member with the given agent address.
func (c *Config) Member(agent ethcommon.Address) (Member, bool) {
	i, ok := slices.BinarySearchFunc(c.Members, agent, func(m Member, a ethcommon.Address) int {
		return m.Agent.Cmp(a)
	})
	if !ok {
		return Member{}, false
	}
	return c.Members[i], true
}

// IsMember reports whether agent is a member.
func (c *Config) IsMember(agent ethcommon.Address) bool {
	_, ok := c.Member(agent)
	return ok
}

// Leader returns the leader of a wave that starts at depth, if depth starts a wave.
func (c *Config) Leader(depth uint64, wavelength int) (ethcommon.Address, bool) {
	first := c.FirstDepth()
	if depth < first || (depth-first)%uint64(wavelength) != 0 {
		return ethcommon.Address{}, false
	}
	wave := (depth - first) / uint64(wavelength)
	return c.Members[wave%uint64(len(c.Members))].Agent, true
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	cp := *c
	cp.Members = slices.Clone(c.Members)
	return &cp
}

// sortMembers orders members by agent address.
func sortMembers(ms []Member) {
	slices.SortFunc(ms, func(a, b Member) int { return a.Agent.Cmp(b.Agent) })
}

// Params are the protocol's tunable rules.
type Params struct {
	// Wavelength is the number of rounds of a wave (w).
	Wavelength int
	// FinalityWindow is how many rounds after a leader's round may hold the
	// blocks that super-ratify it. The prototype uses w (3); the paper w−1.
	FinalityWindow int
}

// DefaultParams are the prototype's rules: w = 3, ratifiers up to r+3.
var DefaultParams = Params{Wavelength: 3, FinalityWindow: 3}

var errMissing = errors.New("missing pointers")
