package execution

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ouripoupko/ibc-go/identity"
)

// Actions a record can carry.
const (
	ActionRegisterAgent  = "register_agent"
	ActionDeployContract = "deploy_contract"
	ActionWrite          = "contract_write"
	ActionJoin           = "join"
	ActionLeave          = "leave"
	ActionRotateKey      = "rotate_key"
	ActionConsent        = "a2a_consent"
)

// ActionForKind maps a transaction kind to the action of its record.
var ActionForKind = map[string]string{
	identity.KindDeploy:    ActionDeployContract,
	identity.KindCall:      ActionWrite,
	identity.KindJoin:      ActionJoin,
	identity.KindLeave:     ActionLeave,
	identity.KindRotateKey: ActionRotateKey,
}

// Record is a request record: what the dispatcher builds from an HTTP
// request, and what consensus hands to execution for each transaction.
type Record struct {
	Type     string `json:"type,omitempty"`
	Action   string `json:"action,omitempty"`
	Contract string `json:"contract,omitempty"`
	Agent    string `json:"agent,omitempty"`
	// Method is the function a request names, in the API (a URL's last
	// part); execution does not read it.
	Method string `json:"method,omitempty"`
	// To and Data are a call's target, a contract in the agreement's world
	// (empty: the root contract), and its calldata, hex.
	To        string  `json:"to,omitempty"`
	Data      string  `json:"data,omitempty"`
	Message   Message `json:"message"`
	Timestamp string  `json:"timestamp,omitempty"`
	HashCode  string  `json:"hash_code,omitempty"`
	// Index is the record's position in its contract's ledger, assigned by
	// consensus (1 = the deploy). 0 means unassigned.
	Index uint64 `json:"index,omitempty"`
	// Tx is the signed transaction the record executes, if any. The ledger
	// keeps it, so every record can be checked against its agent's signature.
	Tx *identity.SignedTx `json:"tx,omitempty"`
}

// Call returns the record's call: its target (nil: the root contract) and
// its calldata.
func (r Record) Call() (*ethcommon.Address, []byte, error) {
	var to *ethcommon.Address
	if r.To != "" {
		if !ethcommon.IsHexAddress(r.To) {
			return nil, nil, fmt.Errorf("invalid call target %q", r.To)
		}
		a := ethcommon.HexToAddress(r.To)
		to = &a
	}
	data, err := hexutil.Decode(r.Data)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid call data: %w", err)
	}
	return to, data, nil
}

// SetCall sets a record's call.
func (r *Record) SetCall(to *ethcommon.Address, data []byte) {
	r.To = ""
	if to != nil {
		r.To = to.Hex()
	}
	r.Data = hexutil.Encode(data)
}

// Message is a record's message. Values is what the code reads; Fields keeps
// every field of the original JSON (for a deploy: the contract's labels,
// code, abi and constructor).
type Message struct {
	Values map[string]any `json:"values,omitempty"`

	// Fields holds all fields of the message as received, numbers as
	// json.Number. Nil for a message built in code.
	Fields map[string]any `json:"-"`
}

type messageFields struct {
	Values map[string]any `json:"values,omitempty"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*m = Message{}
		return nil
	}
	var known messageFields
	if err := decodeNumbers(data, &known); err != nil {
		return err
	}
	var all map[string]any
	if err := decodeNumbers(data, &all); err != nil {
		return err
	}
	*m = Message{Values: known.Values, Fields: all}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	out := maps.Clone(m.Fields)
	if out == nil {
		out = map[string]any{}
	}
	if m.Values != nil {
		out["values"] = m.Values
	}
	return json.Marshal(out)
}

// DecodeRecord parses a JSON record. Numbers are kept as json.Number, so
// integers above 2^53 reach the ABI encoder intact.
func DecodeRecord(data []byte) (Record, error) {
	var r Record
	err := decodeNumbers(data, &r)
	return r, err
}

// DecodeJSON decodes JSON keeping numbers as json.Number.
func DecodeJSON(data []byte, v any) error { return decodeNumbers(data, v) }

func decodeNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
