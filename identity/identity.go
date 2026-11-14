// Package identity holds ibc's signed objects and the one place where their
// signatures are checked (Verify, in envelope.go). An agent has two keys: its
// root key, on the user's device, which signs its registration, its grants
// and its transactions; and its node's key, which signs blocks, and signs
// calls silently only under a grant from the root, which names the contract
// code and the functions. Every message is signed as its EIP-712 digest, in
// an envelope that carries the key and the grant (see docs/keys.md).
package identity

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// The EIP-712 domain of every ibc signature.
const (
	DomainName    = "ibc"
	DomainVersion = "1"
)

// Transaction kinds.
const (
	KindDeploy    = "deploy"
	KindCall      = "call"
	KindJoin      = "join"
	KindLeave     = "leave"
	KindRotateKey = "rotate_key"
)

var (
	ErrBadSignature = errors.New("signature does not match the agent")
	ErrBadKind      = errors.New("unknown transaction kind")
)

// Transaction is something an agent wants to happen in a contract.
type Transaction struct {
	Contract string            `json:"contract"` // contract id; empty for a deploy
	Kind     string            `json:"kind"`
	Agent    ethcommon.Address `json:"agent"`
	Nonce    string            `json:"nonce"`
	Body     string            `json:"body"` // JSON, by kind
}

// SignedTx is a transaction with its signature: by the agent's root key, or by its node under a grant.
type SignedTx struct {
	Tx        Transaction `json:"tx"`
	Signature Envelope    `json:"signature"`
}

// EncodeRLP writes the signed transaction into a block as its JSON text, so
// that blocks carry envelopes of any shape.
func (s SignedTx) EncodeRLP(w io.Writer) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return rlp.Encode(w, raw)
}

// DecodeRLP reads a signed transaction written by EncodeRLP.
func (s *SignedTx) DecodeRLP(st *rlp.Stream) error {
	raw, err := st.Bytes()
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, s)
}

// Registration authorizes a node to act for an agent: to sign blocks of the
// agent's contracts with the node's key. The agent's root key signs it.
type Registration struct {
	Agent   ethcommon.Address `json:"agent"`
	Node    string            `json:"node"`    // the node's URL
	NodeKey ethcommon.Address `json:"nodeKey"` // the address of the node's key
	// The root passkey's credential id, so that the agent can be found from
	// its passkey on a new device; empty for other root keys.
	Credential hexutil.Bytes `json:"credential"`
	Nonce      string        `json:"nonce"`
}

// SignedRegistration is a registration with the root key's signature.
type SignedRegistration struct {
	Registration Registration `json:"registration"`
	Signature    Envelope     `json:"signature"`
}

var domainType = []apitypes.Type{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}

func domain() apitypes.TypedDataDomain {
	return apitypes.TypedDataDomain{Name: DomainName, Version: DomainVersion}
}

// TypedData returns the transaction as EIP-712 typed data.
func (t Transaction) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Transaction": {
				{Name: "contract", Type: "string"},
				{Name: "kind", Type: "string"},
				{Name: "agent", Type: "address"},
				{Name: "nonce", Type: "string"},
				{Name: "body", Type: "string"},
			},
		},
		PrimaryType: "Transaction",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"contract": t.Contract,
			"kind":     t.Kind,
			"agent":    t.Agent.Hex(),
			"nonce":    t.Nonce,
			"body":     t.Body,
		},
	}
}

// Hash returns the transaction's EIP-712 digest, which is its id.
func (t Transaction) Hash() (ethcommon.Hash, error) {
	return typedHash(t.TypedData())
}

// TypedData returns the registration as EIP-712 typed data.
func (r Registration) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Registration": {
				{Name: "agent", Type: "address"},
				{Name: "node", Type: "string"},
				{Name: "nodeKey", Type: "address"},
				{Name: "credential", Type: "bytes"},
				{Name: "nonce", Type: "string"},
			},
		},
		PrimaryType: "Registration",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"agent":      r.Agent.Hex(),
			"node":       r.Node,
			"nodeKey":    r.NodeKey.Hex(),
			"credential": hexutil.Encode(r.Credential),
			"nonce":      r.Nonce,
		},
	}
}

// Hash returns the registration's EIP-712 digest.
func (r Registration) Hash() (ethcommon.Hash, error) {
	return typedHash(r.TypedData())
}

func typedHash(td apitypes.TypedData) (ethcommon.Hash, error) {
	digest, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	return ethcommon.BytesToHash(digest), nil
}

// Verify checks that the transaction is well formed and signed by one of its
// agent's root key, or by the agent's node under a grant, and returns its id.
// A node may sign only calls to the functions its grant names; execution
// then checks the grant's code hash and node against the contract. It uses
// no clock, so every partner reaches the same answer.
func (s SignedTx) Verify() (ethcommon.Hash, error) {
	if err := s.Tx.check(); err != nil {
		return ethcommon.Hash{}, err
	}
	hash, err := s.Tx.Hash()
	if err != nil {
		return ethcommon.Hash{}, err
	}
	signer, err := Verify(hash, s.Signature, ForTransaction)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	if signer.Agent != s.Tx.Agent {
		return ethcommon.Hash{}, ErrBadSignature
	}
	if g := signer.Grant; g != nil {
		if s.Tx.Kind != KindCall {
			return ethcommon.Hash{}, errors.New("a node signs only calls: this transaction needs the root key")
		}
		body, err := s.Tx.ParseBody()
		if err != nil {
			return ethcommon.Hash{}, err
		}
		if method := body.(*CallBody).Method; !g.Allows(method) {
			return ethcommon.Hash{}, fmt.Errorf("the grant does not let the node call %s: it needs the root key", method)
		}
	}
	return hash, nil
}

// Grant returns the grant a node signed this transaction under, or nil if
// the agent's root key signed it. (Call it on a verified transaction.)
func (s SignedTx) Grant() *Grant {
	if s.Signature.Certificate == nil {
		return nil
	}
	return &s.Signature.Certificate.Grant
}

// Verify checks that the registration is signed by its agent's root key, and
// returns its digest and the root key.
func (s SignedRegistration) Verify() (ethcommon.Hash, Signer, error) {
	hash, err := s.Registration.Hash()
	if err != nil {
		return ethcommon.Hash{}, Signer{}, err
	}
	if s.Signature.Certificate != nil {
		return ethcommon.Hash{}, Signer{}, errors.New("a registration is signed by the root key")
	}
	signer, err := Verify(hash, s.Signature, ForRoot)
	if err != nil {
		return ethcommon.Hash{}, Signer{}, err
	}
	if signer.Agent != s.Registration.Agent {
		return ethcommon.Hash{}, Signer{}, ErrBadSignature
	}
	return hash, signer, nil
}

func (t Transaction) check() error {
	if t.Agent == (ethcommon.Address{}) {
		return errors.New("transaction has no agent")
	}
	switch t.Kind {
	case KindDeploy:
		if t.Contract != "" {
			return errors.New("a deploy has no contract")
		}
	case KindCall, KindJoin, KindLeave, KindRotateKey:
		if t.Contract == "" {
			return fmt.Errorf("a %s needs a contract", t.Kind)
		}
	default:
		return fmt.Errorf("%w %q", ErrBadKind, t.Kind)
	}
	_, err := t.ParseBody()
	return err
}

// Bodies of the transaction kinds.
type (
	DeployBody struct {
		Name        string            `json:"name,omitempty"`
		Contract    string            `json:"contract,omitempty"`
		Protocol    string            `json:"protocol,omitempty"`
		DefaultApp  string            `json:"default_app,omitempty"`
		Profile     any               `json:"profile,omitempty"`
		Code        string            `json:"code"`
		ABI         json.RawMessage   `json:"abi"`
		Constructor map[string]any    `json:"constructor,omitempty"`
		Node        string            `json:"node"`
		NodeKey     ethcommon.Address `json:"nodeKey"`
		// The Solidity source, and how the creator's node compiled Code from
		// it (see package compiler): a joining node compiles it again and
		// compares. Partners join, and grant, by the source's hash.
		Source   string          `json:"source"`
		Compiler json.RawMessage `json:"compiler"`
	}
	CallBody struct {
		Method string         `json:"method"`
		Values map[string]any `json:"values,omitempty"`
	}
	JoinBody struct {
		Node    string            `json:"node"`
		NodeKey ethcommon.Address `json:"nodeKey"`
		Profile any               `json:"profile,omitempty"`
		// Guest joins as a guest partner: one that takes no part in
		// consensus, and whose node sends its transactions and reads to the
		// contract's hosts (its full partners). Host is the member whose
		// node the guest joined through; it keeps the guest informed.
		Guest bool              `json:"guest,omitempty"`
		Host  ethcommon.Address `json:"host,omitempty"`
		// SourceHash is the SourceHash of the contract's source as the
		// joining agent read it. Partners refuse the join if it is not the
		// contract's.
		SourceHash ethcommon.Hash `json:"sourceHash"`
	}
	LeaveBody struct {
		Member ethcommon.Address `json:"member"`
	}
	RotateKeyBody struct {
		Node    string            `json:"node"` // the URL of the agent's new node
		NodeKey ethcommon.Address `json:"nodeKey"`
	}
)

// ParseBody decodes the body for the transaction's kind. Numbers are kept
// as json.Number.
func (t Transaction) ParseBody() (any, error) {
	var body any
	switch t.Kind {
	case KindDeploy:
		body = &DeployBody{}
	case KindCall:
		body = &CallBody{}
	case KindJoin:
		body = &JoinBody{}
	case KindLeave:
		body = &LeaveBody{}
	case KindRotateKey:
		body = &RotateKeyBody{}
	default:
		return nil, fmt.Errorf("%w %q", ErrBadKind, t.Kind)
	}
	dec := json.NewDecoder(strings.NewReader(t.Body))
	dec.UseNumber()
	if err := dec.Decode(body); err != nil {
		return nil, fmt.Errorf("%s body: %w", t.Kind, err)
	}
	switch b := body.(type) {
	case *DeployBody:
		if b.Code == "" || len(b.ABI) == 0 || b.NodeKey == (ethcommon.Address{}) {
			return nil, errors.New("deploy body needs code, abi and nodeKey")
		}
		if b.Source == "" || len(b.Compiler) == 0 || string(b.Compiler) == "null" {
			return nil, errors.New("deploy body needs the source and the compiler that compiled it")
		}
	case *CallBody:
		if b.Method == "" {
			return nil, errors.New("call body needs a method")
		}
	case *JoinBody:
		if b.NodeKey == (ethcommon.Address{}) || b.Node == "" {
			return nil, errors.New("join body needs node and nodeKey")
		}
		if b.Guest && b.Host == (ethcommon.Address{}) {
			return nil, errors.New("a guest's join body needs its host")
		}
		if b.SourceHash == (ethcommon.Hash{}) {
			return nil, errors.New("join body needs the sourceHash of the contract's source")
		}
	case *LeaveBody:
		if b.Member == (ethcommon.Address{}) {
			return nil, errors.New("leave body needs a member")
		}
	case *RotateKeyBody:
		if b.NodeKey == (ethcommon.Address{}) || b.Node == "" {
			return nil, errors.New("rotate_key body needs node and nodeKey")
		}
	}
	return body, nil
}

// NewNonce returns a random nonce.
func NewNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// SignHash signs a digest the way wallets do: 65 bytes, v = 27 or 28.
func SignHash(hash ethcommon.Hash, key *ecdsa.PrivateKey) ([]byte, error) {
	sig, err := crypto.Sign(hash[:], key)
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

// Recover returns the address that signed hash. It accepts v = 0/1 and 27/28.
func Recover(hash ethcommon.Hash, sig []byte) (ethcommon.Address, error) {
	if len(sig) != 65 {
		return ethcommon.Address{}, fmt.Errorf("signature must be 65 bytes, got %d", len(sig))
	}
	s := make([]byte, 65)
	copy(s, sig)
	if s[64] >= 27 {
		s[64] -= 27
	}
	if s[64] > 1 {
		return ethcommon.Address{}, errors.New("invalid signature recovery id")
	}
	pub, err := crypto.SigToPub(hash[:], s)
	if err != nil {
		return ethcommon.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}

// SignTx signs a transaction (the command line and tests; apps sign in the
// browser).
func SignTx(t Transaction, s KeySigner) (SignedTx, error) {
	hash, err := t.Hash()
	if err != nil {
		return SignedTx{}, err
	}
	env, err := s.SignDigest(hash)
	if err != nil {
		return SignedTx{}, err
	}
	return SignedTx{Tx: t, Signature: env}, nil
}

// SignRegistration signs a registration with a root key (the command line
// and tests).
func SignRegistration(r Registration, s KeySigner) (SignedRegistration, error) {
	hash, err := r.Hash()
	if err != nil {
		return SignedRegistration{}, err
	}
	env, err := s.SignDigest(hash)
	if err != nil {
		return SignedRegistration{}, err
	}
	return SignedRegistration{Registration: r, Signature: env}, nil
}

// Address returns the address of a secp256k1 key, such as a node key.
func Address(key *ecdsa.PrivateKey) ethcommon.Address {
	return crypto.PubkeyToAddress(key.PublicKey)
}

// LoadOrCreateNodeKey reads the node's key from path, creating it (readable
// by the owner only) if it does not exist.
func LoadOrCreateNodeKey(path string) (*ecdsa.PrivateKey, error) {
	key, err := crypto.LoadECDSA(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("node key %s: %w", path, err)
	}
	key, err = crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	if err := crypto.SaveECDSA(path, key); err != nil {
		return nil, fmt.Errorf("save node key %s: %w", path, err)
	}
	return key, nil
}

// SourceHash is a contract's source hash, as joins, grants and consents name
// it: the keccak256 of the Solidity source text, byte for byte.
func SourceHash(source string) ethcommon.Hash {
	return crypto.Keccak256Hash([]byte(source))
}

// CodeHash is the keccak256 of a contract's creation bytecode (hex, with or
// without 0x).
func CodeHash(code string) (ethcommon.Hash, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(code), "0x"))
	if err != nil || len(raw) == 0 {
		return ethcommon.Hash{}, errors.New("code must be hex bytecode")
	}
	return crypto.Keccak256Hash(raw), nil
}
