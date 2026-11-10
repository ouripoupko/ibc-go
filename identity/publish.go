package identity

import (
	"errors"
	"strconv"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Publish lets anyone read the agent's copy of a contract (public_read):
// unsigned reads of its view and pure functions, which run with msg.sender
// the zero address. Public false stops it. The root key signs it; it stays on
// the agent's node (partners never see it). Time (unix milliseconds) orders
// the agent's choices: the node takes only a newer one than it has.
type Publish struct {
	Agent    ethcommon.Address `json:"agent"`
	Contract string            `json:"contract"` // the contract id
	Public   bool              `json:"public"`
	Time     uint64            `json:"time"` // unix milliseconds
	Nonce    string            `json:"nonce"`
}

// SignedPublish is a publish with the root key's signature.
type SignedPublish struct {
	Publish   Publish  `json:"publish"`
	Signature Envelope `json:"signature"`
}

// TypedData returns the publish as EIP-712 typed data.
func (p Publish) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Publish": {
				{Name: "agent", Type: "address"},
				{Name: "contract", Type: "string"},
				{Name: "public", Type: "bool"},
				{Name: "time", Type: "uint64"},
				{Name: "nonce", Type: "string"},
			},
		},
		PrimaryType: "Publish",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"agent":    p.Agent.Hex(),
			"contract": p.Contract,
			"public":   p.Public,
			"time":     strconv.FormatUint(p.Time, 10),
			"nonce":    p.Nonce,
		},
	}
}

// Hash returns the publish's EIP-712 digest.
func (p Publish) Hash() (ethcommon.Hash, error) {
	return typedHash(p.TypedData())
}

// Verify checks that the agent's root key signed the publish.
func (s SignedPublish) Verify() error {
	if s.Signature.Certificate != nil {
		return errors.New("a publish is signed by the root key")
	}
	if s.Publish.Contract == "" {
		return errors.New("publish: no contract")
	}
	h, err := s.Publish.Hash()
	if err != nil {
		return err
	}
	signer, err := Verify(h, s.Signature, ForRoot)
	if err != nil {
		return err
	}
	if signer.Agent != s.Publish.Agent {
		return ErrBadSignature
	}
	return nil
}

// SignPublish signs a publish with the root key (the command line and tests).
func SignPublish(p Publish, root KeySigner) (SignedPublish, error) {
	h, err := p.Hash()
	if err != nil {
		return SignedPublish{}, err
	}
	env, err := root.SignDigest(h)
	if err != nil {
		return SignedPublish{}, err
	}
	return SignedPublish{Publish: p, Signature: env}, nil
}
