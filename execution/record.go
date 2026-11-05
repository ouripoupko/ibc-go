package execution

import (
	"bytes"
	"encoding/json"
	"maps"
)

// Actions a record can carry (the 'action' query parameter in Python).
const (
	ActionRegisterAgent  = "register_agent"
	ActionDeployContract = "deploy_contract"
	ActionConnect        = "a2a_connect"
	ActionWrite          = "contract_write"
	ActionPartner        = "int_partner"
	ActionReplyJoin      = "a2a_reply_join"
	ActionConsent        = "a2a_consent"
)

// Record is a request record (the dict built in Python's ibc.py).
type Record struct {
	Type      string  `json:"type,omitempty"`
	Action    string  `json:"action,omitempty"`
	Contract  string  `json:"contract,omitempty"`
	Agent     string  `json:"agent,omitempty"`
	Method    string  `json:"method,omitempty"`
	Message   Message `json:"message"`
	Timestamp string  `json:"timestamp,omitempty"`
	HashCode  string  `json:"hash_code,omitempty"`
	// Index is the record's position in its contract's ledger, assigned by
	// consensus (1 = the deploy record). 0 means unassigned.
	Index uint64 `json:"index,omitempty"`
	// Status is the outcome of a join, set on the int_partner record that
	// execution hands back to consensus.
	Status *bool `json:"status,omitempty"`
}

// Message is a record's message. Values, From, To and Msg are the fields the
// code reads; Fields keeps every field of the original JSON, so that a
// deploy message (code, abi, constructor, name, protocol, ...) and the
// ledger keep everything that was sent.
type Message struct {
	Values map[string]any `json:"values,omitempty"`
	From   string         `json:"from,omitempty"`
	To     string         `json:"to,omitempty"`
	Msg    *JoinMsg       `json:"msg,omitempty"`

	// Fields holds all fields of the message as received, numbers as
	// json.Number. Nil for a message built in code.
	Fields map[string]any `json:"-"`
}

// messageFields mirrors Message without its methods, for decoding.
type messageFields struct {
	Values map[string]any `json:"values,omitempty"`
	From   string         `json:"from,omitempty"`
	To     string         `json:"to,omitempty"`
	Msg    *JoinMsg       `json:"msg,omitempty"`
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
	*m = Message{Values: known.Values, From: known.From, To: known.To, Msg: known.Msg, Fields: all}
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
	if m.From != "" {
		out["from"] = m.From
	}
	if m.To != "" {
		out["to"] = m.To
	}
	if m.Msg != nil {
		out["msg"] = m.Msg
	}
	return json.Marshal(out)
}

// JoinMsg identifies the agent asking to join a contract (a2a_connect),
// or answering such a request (a2a_reply_join, with Status).
type JoinMsg struct {
	PID     string `json:"pid"`
	Address string `json:"address"`
	Profile any    `json:"profile,omitempty"`
	Status  *bool  `json:"status,omitempty"`
}

// DecodeRecord parses a JSON record. Numbers are kept as json.Number, so
// integers above 2^53 reach the ABI encoder intact.
func DecodeRecord(data []byte) (Record, error) {
	var r Record
	err := decodeNumbers(data, &r)
	return r, err
}

func decodeNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
