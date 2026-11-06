package identity

import (
	"crypto/ecdsa"
	"errors"
	"strconv"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Query is a signed request to see something on a node: a contract read, a
// contract's details, the agent's contracts, or the event stream. It never
// reaches consensus or a ledger. Its EIP-712 type differs from Transaction,
// so a signed query can never pass for a transaction.
type Query struct {
	Agent    ethcommon.Address `json:"agent"`    // who asks: the signer
	Action   string            `json:"action"`   // the API action, e.g. contract_read
	Target   string            `json:"target"`   // the agent whose copy is asked (the URL's agent)
	Contract string            `json:"contract"` // the contract, or empty
	Body     string            `json:"body"`     // JSON, by action; empty if none
	Time     uint64            `json:"time"`     // unix seconds when signed
	Nonce    string            `json:"nonce"`
}

// SignedQuery is a query with the agent's signature.
type SignedQuery struct {
	Query     Query         `json:"query"`
	Signature hexutil.Bytes `json:"signature"`
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

// Verify checks that the query is signed by its agent, and returns its digest.
func (s SignedQuery) Verify() (ethcommon.Hash, error) {
	if s.Query.Agent == (ethcommon.Address{}) {
		return ethcommon.Hash{}, errors.New("query has no agent")
	}
	hash, err := s.Query.Hash()
	if err != nil {
		return ethcommon.Hash{}, err
	}
	signer, err := Recover(hash, s.Signature)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	if signer != s.Query.Agent {
		return ethcommon.Hash{}, ErrBadSignature
	}
	return hash, nil
}

// SignQuery signs a query with a root key (tests and tools).
func SignQuery(q Query, key *ecdsa.PrivateKey) (SignedQuery, error) {
	hash, err := q.Hash()
	if err != nil {
		return SignedQuery{}, err
	}
	sig, err := SignHash(hash, key)
	if err != nil {
		return SignedQuery{}, err
	}
	return SignedQuery{Query: q, Signature: sig}, nil
}
