// Package consensus orders a contract's records before they are executed.
//
// The Python ibc implemented PBFT here (consensus/pbft.py). The Go version
// will get a new protocol; until then, Immediate accepts every record at
// once.
package consensus

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/ouripoupko/ibc-go/execution"
)

var (
	// ErrUnknownAction is returned for a record consensus does not handle.
	ErrUnknownAction = errors.New("consensus: unknown action")
	// ErrNotSupported is returned for protocol messages (a2a_consent) that
	// the current protocol does not use.
	ErrNotSupported = errors.New("consensus: not supported by this protocol")
)

// Protocol is a consensus protocol. The dispatcher hands it every record
// that needs agreement among a contract's partners: deploy_contract,
// a2a_connect and contract_write from clients and partners, and the
// protocol's own messages (a2a_consent, a2a_reply_join). register_agent is
// not a consensus matter and goes to execution directly, as in Python.
type Protocol interface {
	// Submit receives a record for agent's node. It returns once the
	// record is accepted for processing; the outcome reaches the agent's
	// clients later, through execution's replies.
	Submit(agent string, record execution.Record) error
	// PartnerUpdate receives the outcome of a join from execution (an
	// int_partner record with Status set), so the protocol can update the
	// set of partners it runs with.
	PartnerUpdate(agent string, record execution.Record)
	// Close stops the protocol.
	Close()
}

// Executor receives committed records, in order; execution.Navigator
// implements it.
type Executor interface {
	Submit(agent string, record execution.Record) error
}

// Publisher delivers a reply to an agent's clients.
type Publisher interface {
	Publish(agent string, reply execution.Reply)
}

// Immediate is a stub Protocol that commits every record the moment it
// arrives, in arrival order, without talking to any partner.
//
// It runs no agreement at all: each node executes only the records it
// receives itself, so partners of a contract do not see each other's
// writes. It exists so the rest of the node can run until the new protocol
// is written.
type Immediate struct {
	executor  Executor
	publisher Publisher
}

// NewImmediate returns an Immediate protocol that hands records to executor
// and protocol replies to publisher (which may be nil).
func NewImmediate(executor Executor, publisher Publisher) *Immediate {
	return &Immediate{executor: executor, publisher: publisher}
}

func (p *Immediate) Submit(agent string, record execution.Record) error {
	switch record.Action {
	case execution.ActionDeployContract, execution.ActionConnect, execution.ActionWrite:
		// The ledger position is assigned by arrival order. An index sent
		// by someone else means nothing to this stub.
		record.Index = 0
		return p.executor.Submit(agent, record)

	case execution.ActionReplyJoin:
		return p.replyJoin(agent, record)

	case execution.ActionConsent:
		return fmt.Errorf("%w: %s", ErrNotSupported, record.Action)
	}
	return fmt.Errorf("%w %q", ErrUnknownAction, record.Action)
}

// replyJoin handles a partner's answer to this agent's join request. As in
// Python, the answer is published to the agent's clients.
func (p *Immediate) replyJoin(agent string, record execution.Record) error {
	status := false
	if msg := record.Message.Msg; msg != nil && msg.Status != nil {
		status = *msg.Status
	}
	if status {
		// TODO: on approval, Python fetched the partner's ledger
		// (a2a_get_ledger) and replayed its records to build the contract
		// locally. Syncing a joining partner belongs to the new protocol.
		// Joins are never approved yet (approve_partner is a stub), so this
		// path is not reached.
		slog.Warn("join approved but ledger sync is not implemented",
			"agent", agent, "contract", record.Contract)
	}
	if p.publisher != nil {
		p.publisher.Publish(agent, execution.Reply{Action: record.Action, Reply: status})
	}
	return nil
}

// PartnerUpdate does nothing: the stub never talks to partners.
func (p *Immediate) PartnerUpdate(string, execution.Record) {}

// Close does nothing; the stub holds no resources.
func (p *Immediate) Close() {}
