// Package consensus orders each contract's transactions before they are
// executed. Each contract chooses its protocol when it is deployed:
// Cordial Miners (package cordial), the default, or "immediate", which
// orders a single creator's transactions locally and exists for testing.
package consensus

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// Protocol names, as given in a deploy's "protocol" field.
const (
	ProtocolCordial   = "cordial"
	ProtocolImmediate = "immediate"
)

var (
	// ErrUnknownProtocol is returned for a deploy naming a protocol this node does not run.
	ErrUnknownProtocol = errors.New("consensus: unknown protocol")
	// ErrUnknownContract is returned for a transaction to a contract this agent does not have.
	ErrUnknownContract = errors.New("consensus: unknown contract")
)

// Protocol orders transactions. The dispatcher gives it clients' signed
// transactions and other nodes' messages; execution reports decisions and
// progress back to it.
type Protocol interface {
	// SubmitTx accepts a transaction signed by agent, and returns its id.
	SubmitTx(agent string, stx identity.SignedTx) (string, error)
	// RelayJoin sends agent's join transaction to a member's node.
	RelayJoin(ctx context.Context, agent string, stx identity.SignedTx, memberNode, memberAgent string) (string, error)
	// Receive handles a message from another node; the result is the reply.
	Receive(ctx context.Context, agent, contract string, body []byte) (any, error)
	// Read runs a read of a contract agent is a guest of, on a host.
	Read(ctx context.Context, agent, contract, method string, values map[string]any) (any, error)
	// Decided, Applied and Published are execution's reports.
	Decided(agent, contract string, record execution.Record, approved bool)
	Applied(agent, contract string, index uint64, stateRoot string)
	Published(agent string, reply execution.Reply)
	// Close stops the protocol.
	Close()
}

// Execution is what consensus needs from execution.
type Execution interface {
	Submit(agent string, record execution.Record) error
	Decision(agent, contract, id string) (approved, known bool, err error)
	AgentExists(agent string) (bool, error)
	// Progress is the last applied index and the state root after it.
	Progress(agent, contract string) (index uint64, stateRoot string, err error)
	// Ledger is the contract's records with index >= from, in order.
	Ledger(agent, contract string, from uint64) ([]execution.LedgerEntry, error)
	// DeployRecord is the contract's first record.
	DeployRecord(agent, contract string) (json.RawMessage, error)
	// Read runs a read-only call as caller.
	Read(agent, caller string, record execution.Record) (common.Result, error)

	// A guest's node keeps the contracts its agents are guests of.
	Guest(agent, contract string) (execution.GuestEntry, bool, error)
	StoreGuest(agent, contract string, deploy map[string]any, e execution.GuestEntry) error
	SetHosts(agent, contract string, hosts []execution.Host) error
	RemoveGuest(agent, contract string) error
	// A host's node knows the guests of its agents' contracts.
	Guests(agent, contract string) ([]execution.Guest, error)
	GuestOf(agent, contract, guest string) (execution.Guest, bool, error)
}

// Publisher delivers a reply to an agent's clients.
type Publisher interface {
	Publish(agent string, reply execution.Reply)
}

// protocolOf maps a deploy's protocol field to a protocol name.
func protocolOf(field string) (string, error) {
	switch field {
	case "", ProtocolCordial, "BFT":
		return ProtocolCordial, nil
	case ProtocolImmediate:
		return ProtocolImmediate, nil
	}
	return "", ErrUnknownProtocol
}
