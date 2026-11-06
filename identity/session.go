package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// SessionScope is the only scope a session can have: reading. A session key
// signs queries (contract reads, contract details, the event stream), never
// transactions, so every ledger entry stays signed by an agent's own key.
const SessionScope = "read"

// Session lets a temporary key sign an agent's queries on one node until a
// time. The agent signs it once, in its wallet; the app then signs each
// query with the session key, without asking. The session key is a P-256
// key, the kind browsers make and keep (WebCrypto), so the app needs no
// Ethereum library and can keep the key unextractable.
type Session struct {
	Agent   ethcommon.Address `json:"agent"`
	NodeKey ethcommon.Address `json:"nodeKey"` // the node it is for: its key, from node_info
	Key     hexutil.Bytes     `json:"key"`     // the session's P-256 public key, uncompressed (65 bytes)
	Scope   string            `json:"scope"`   // SessionScope
	Expires uint64            `json:"expires"` // unix seconds
	Nonce   string            `json:"nonce"`
}

// SignedSession is a session with the agent's signature.
type SignedSession struct {
	Session   Session       `json:"session"`
	Signature hexutil.Bytes `json:"signature"`
}

// TypedData returns the session as EIP-712 typed data.
func (s Session) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Session": {
				{Name: "agent", Type: "address"},
				{Name: "nodeKey", Type: "address"},
				{Name: "key", Type: "bytes"},
				{Name: "scope", Type: "string"},
				{Name: "expires", Type: "uint64"},
				{Name: "nonce", Type: "string"},
			},
		},
		PrimaryType: "Session",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"agent":   s.Agent.Hex(),
			"nodeKey": s.NodeKey.Hex(),
			"key":     hexutil.Encode(s.Key),
			"scope":   s.Scope,
			"expires": strconv.FormatUint(s.Expires, 10),
			"nonce":   s.Nonce,
		},
	}
}

// Hash returns the session's EIP-712 digest.
func (s Session) Hash() (ethcommon.Hash, error) {
	return typedHash(s.TypedData())
}

// PublicKey parses the session key.
func (s Session) PublicKey() (*ecdsa.PublicKey, error) {
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), s.Key)
	if err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}
	return pub, nil
}

// Verify checks that the session is well formed and signed by its agent.
// The node checks the rest: that it is for this node and not expired.
func (s SignedSession) Verify() error {
	if s.Session.Scope != SessionScope {
		return fmt.Errorf("session scope %q: only %q is allowed", s.Session.Scope, SessionScope)
	}
	if _, err := s.Session.PublicKey(); err != nil {
		return err
	}
	hash, err := s.Session.Hash()
	if err != nil {
		return err
	}
	signer, err := Recover(hash, s.Signature)
	if err != nil {
		return err
	}
	if signer != s.Session.Agent {
		return fmt.Errorf("session: %w", ErrBadSignature)
	}
	return nil
}

// SessionMessage is what a session key signs for a query: its fields, one
// per line, so that no JSON encoding has to match byte for byte. The agent
// is written in lower case, as wallets give it.
func (q Query) SessionMessage() []byte {
	return []byte("ibc query v1\n" + strings.ToLower(q.Agent.Hex()) + "\n" + q.Action + "\n" + q.Target + "\n" + q.Contract +
		"\n" + q.Body + "\n" + strconv.FormatUint(q.Time, 10) + "\n" + q.Nonce)
}

// verifySession checks a query signed with a session key, and returns the
// digest that identifies the query.
func (s SignedQuery) verifySession() (ethcommon.Hash, error) {
	if s.Session.Session.Agent != s.Query.Agent {
		return ethcommon.Hash{}, errors.New("session of another agent")
	}
	if err := s.Session.Verify(); err != nil {
		return ethcommon.Hash{}, err
	}
	pub, _ := s.Session.Session.PublicKey()
	digest := sha256.Sum256(s.Query.SessionMessage())
	if len(s.Signature) != 64 {
		return ethcommon.Hash{}, errors.New("a session signature is 64 bytes (r and s)")
	}
	r, sv := new(big.Int).SetBytes(s.Signature[:32]), new(big.Int).SetBytes(s.Signature[32:])
	if !ecdsa.Verify(pub, digest[:], r, sv) {
		return ethcommon.Hash{}, errors.New("query: signature does not match the session key")
	}
	return ethcommon.Hash(digest), nil
}

// SignWithSession signs a query with a session key (tests and tools; apps
// use WebCrypto).
func SignWithSession(q Query, session SignedSession, key *ecdsa.PrivateKey) (SignedQuery, error) {
	digest := sha256.Sum256(q.SessionMessage())
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return SignedQuery{}, err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return SignedQuery{Query: q, Signature: sig, Session: &session}, nil
}

// SignSession signs a session with the agent's root key (tests and tools).
func SignSession(s Session, key *ecdsa.PrivateKey) (SignedSession, error) {
	hash, err := s.Hash()
	if err != nil {
		return SignedSession{}, err
	}
	sig, err := SignHash(hash, key)
	if err != nil {
		return SignedSession{}, err
	}
	return SignedSession{Session: s, Signature: sig}, nil
}
