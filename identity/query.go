package identity

import (
	"errors"
	"strconv"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Query is a signed request to the agent's node, by the agent's root key or by
// an app the agent connected: a contract read, a contract's details, the
// agent's contracts, the event stream, or, from an app, a call for the node
// to sign silently. It never reaches consensus or a ledger. Its EIP-712 type
// differs from Transaction, so a signed query can never pass for a
// transaction.
type Query struct {
	Agent    ethcommon.Address `json:"agent"`    // who asks: the signer
	Action   string            `json:"action"`   // the API action, e.g. contract_read
	Target   string            `json:"target"`   // the agent whose copy is asked (the URL's agent)
	Contract string            `json:"contract"` // the contract, or empty
	Body     string            `json:"body"`     // JSON, by action; empty if none
	Time     uint64            `json:"time"`     // unix seconds when signed
	Nonce    string            `json:"nonce"`
}

// SignedQuery is a query with its signature: by the agent's root key, or by
// the key of an app the agent connected.
type SignedQuery struct {
	Query     Query    `json:"query"`
	Signature Envelope `json:"signature"`
}

// TypedData returns the query as EIP-712 typed data.
func (q Query) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Query": {
				{Name: "agent", Type: "address"},
				{Name: "action", Type: "string"},
				{Name: "target", Type: "string"},
				{Name: "contract", Type: "string"},
				{Name: "body", Type: "string"},
				{Name: "time", Type: "uint64"},
				{Name: "nonce", Type: "string"},
			},
		},
		PrimaryType: "Query",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"agent":    q.Agent.Hex(),
			"action":   q.Action,
			"target":   q.Target,
			"contract": q.Contract,
			"body":     q.Body,
			"time":     strconv.FormatUint(q.Time, 10),
			"nonce":    q.Nonce,
		},
	}
}

// Hash returns the query's EIP-712 digest.
func (q Query) Hash() (ethcommon.Hash, error) {
	return typedHash(q.TypedData())
}

// Verify checks the query's signature, and returns its digest and the key
// that signed it. Whose key it is (the agent's root, or an app the agent
// connected) is for the node to decide.
func (s SignedQuery) Verify() (ethcommon.Hash, Signer, error) {
	if s.Query.Agent == (ethcommon.Address{}) {
		return ethcommon.Hash{}, Signer{}, errors.New("query has no agent")
	}
	if s.Signature.Certificate != nil {
		return ethcommon.Hash{}, Signer{}, errors.New("a query carries no grant")
	}
	hash, err := s.Query.Hash()
	if err != nil {
		return ethcommon.Hash{}, Signer{}, err
	}
	signer, err := Verify(hash, s.Signature, ForQuery)
	if err != nil {
		return ethcommon.Hash{}, Signer{}, err
	}
	return hash, signer, nil
}

// SignQuery signs a query (the command line and tests).
func SignQuery(q Query, s KeySigner) (SignedQuery, error) {
	hash, err := q.Hash()
	if err != nil {
		return SignedQuery{}, err
	}
	env, err := s.SignDigest(hash)
	if err != nil {
		return SignedQuery{}, err
	}
	return SignedQuery{Query: q, Signature: env}, nil
}
