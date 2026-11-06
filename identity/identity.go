// Package identity holds ibc's signed objects: transactions and agent
// registrations, signed by an agent's root key with EIP-712 (the format of
// MetaMask's eth_signTypedData_v4), and the node's own key, which signs
// blocks. Nodes never hold an agent's root key.
package identity

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
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

// SignedTx is a transaction with the agent's signature.
type SignedTx struct {
	Tx        Transaction   `json:"tx"`
	Signature hexutil.Bytes `json:"signature"`
}

// Registration authorizes a node to act for an agent: to sign blocks of the
// agent's contracts with the node's key.
type Registration struct {
	Agent   ethcommon.Address `json:"agent"`
	Node    string            `json:"node"`    // the node's URL
	NodeKey ethcommon.Address `json:"nodeKey"` // the address of the node's key
	Nonce   string            `json:"nonce"`
}

// SignedRegistration is a registration with the agent's signature.
type SignedRegistration struct {
	Registration Registration  `json:"registration"`
	Signature    hexutil.Bytes `json:"signature"`
}

var domainType = []apitypes.Type{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}

func domain() apitypes.TypedDataDomain {
	return apitypes.TypedDataDomain{Name: DomainName, Version: DomainVersion}
}

// TypedData returns the transaction as EIP-712 typed data, as a wallet signs it.
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
				{Name: "nonce", Type: "string"},
			},
		},
		PrimaryType: "Registration",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"agent":   r.Agent.Hex(),
			"node":    r.Node,
			"nodeKey": r.NodeKey.Hex(),
			"nonce":   r.Nonce,
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

// Verify checks that the transaction is well formed and signed by its agent,
// and returns its id.
func (s SignedTx) Verify() (ethcommon.Hash, error) {
	if err := s.Tx.check(); err != nil {
		return ethcommon.Hash{}, err
	}
	hash, err := s.Tx.Hash()
	if err != nil {
		return ethcommon.Hash{}, err
	}
	signer, err := Recover(hash, s.Signature)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	if signer != s.Tx.Agent {
		return ethcommon.Hash{}, ErrBadSignature
	}
	return hash, nil
}

// Verify checks that the registration is signed by its agent.
func (s SignedRegistration) Verify() (ethcommon.Hash, error) {
	hash, err := s.Registration.Hash()
	if err != nil {
		return ethcommon.Hash{}, err
	}
	signer, err := Recover(hash, s.Signature)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	if signer != s.Registration.Agent {
		return ethcommon.Hash{}, ErrBadSignature
	}
	return hash, nil
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
	}
	CallBody struct {
		Method string         `json:"method"`
		Values map[string]any `json:"values,omitempty"`
	}
	JoinBody struct {
		Node    string            `json:"node"`
		NodeKey ethcommon.Address `json:"nodeKey"`
		Profile any               `json:"profile,omitempty"`
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
	case *CallBody:
		if b.Method == "" {
			return nil, errors.New("call body needs a method")
		}
	case *JoinBody:
		if b.NodeKey == (ethcommon.Address{}) || b.Node == "" {
			return nil, errors.New("join body needs node and nodeKey")
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

// Sign signs a transaction with a root key. Wallets do this for real users;
// this is for tests and command-line tools.
func Sign(t Transaction, key *ecdsa.PrivateKey) (SignedTx, error) {
	hash, err := t.Hash()
	if err != nil {
		return SignedTx{}, err
	}
	sig, err := SignHash(hash, key)
	if err != nil {
		return SignedTx{}, err
	}
	return SignedTx{Tx: t, Signature: sig}, nil
}

// SignRegistration signs a registration with a root key (tests and tools).
func SignRegistration(r Registration, key *ecdsa.PrivateKey) (SignedRegistration, error) {
	hash, err := r.Hash()
	if err != nil {
		return SignedRegistration{}, err
	}
	sig, err := SignHash(hash, key)
	if err != nil {
		return SignedRegistration{}, err
	}
	return SignedRegistration{Registration: r, Signature: sig}, nil
}

// Address returns the address of a key.
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
