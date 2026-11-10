// Package execution runs contracts: ContractExecution applies a contract's
// ordered records to its EVM state, and the Navigator gives each contract
// its own goroutine, fed from the contract's ledger.
package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/identity"
)

// PartnersCollection is the name of the contract document's partners
// sub-collection.
const PartnersCollection = "pda_partners"

// ErrAlreadyCreated is returned by Create for a contract that already has
// its deploy record applied.
var ErrAlreadyCreated = errors.New("contract already created")

// ErrGrantDoesNotApply is the outcome of a call the agent's node signed under
// a grant that does not fit this contract: made for another code, or for a
// node that is no longer the agent's node here. The call is recorded and
// changes nothing.
var ErrGrantDoesNotApply = errors.New("the node's grant does not apply")

// ErrNotPartner is the outcome of a call from an agent that is neither a
// partner nor a guest of the contract. The call is recorded and changes
// nothing.
var ErrNotPartner = errors.New("not a partner or guest of this contract")

// Partner roles, in the partners collection.
const (
	RolePartner = "partner"
	RoleGuest   = "guest"
)

// Ledger is a contract's ordered list of records.
type Ledger interface {
	// Log appends a record.
	Log(record Record) error
	// Records calls fn for every record with index >= from (1-based), in order.
	Records(from uint64, fn func(index uint64, record Record) error) error
}

// ChainLedger is a Ledger stored in the node's database.
type ChainLedger struct {
	Ledger *common.ContractLedger
}

func (l ChainLedger) Log(record Record) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = l.Ledger.Append(raw)
	return err
}

func (l ChainLedger) Records(from uint64, fn func(uint64, Record) error) error {
	return l.Ledger.Records(from, func(index uint64, raw json.RawMessage) error {
		record, err := DecodeRecord(raw)
		if err != nil {
			return fmt.Errorf("ledger record %d: %w", index, err)
		}
		return fn(index, record)
	})
}

// ContractExecution is one contract instance on this agent.
//
// The contract's block number is its cursor: the index of the last ledger
// record applied. Every logged record is applied exactly once and advances
// it by one, whatever the outcome, so Replay knows where to resume.
type ContractExecution struct {
	contractDoc common.Document
	hashCode    string
	me          string
	ledger      Ledger
	partnersDB  common.Collection
	state       *common.State
}

// NewContractExecution creates the execution of contract hashCode for agent
// me. Its EVM state lives in db, in the contract's own key range.
func NewContractExecution(contractDoc common.Document, hashCode, me string, db ethdb.Database, ledger Ledger) *ContractExecution {
	partnersDB := contractDoc.GetSubCollection(PartnersCollection)
	return &ContractExecution{
		contractDoc: contractDoc,
		hashCode:    hashCode,
		me:          me,
		ledger:      ledger,
		partnersDB:  partnersDB,
		state:       common.NewState(contractDoc, partnersDB, common.ContractDatabase(db, me, hashCode)),
	}
}

// State returns the contract's state.
func (c *ContractExecution) State() *common.State { return c.state }

// Create logs and applies the record that created the contract.
func (c *ContractExecution) Create(record Record) (bool, error) {
	cursor, err := c.cursor()
	if err != nil {
		return false, err
	}
	if cursor > 0 {
		return false, ErrAlreadyCreated
	}
	if err := c.ledger.Log(record); err != nil {
		return false, err
	}
	return c.create(record)
}

func (c *ContractExecution) create(record Record) (bool, error) {
	if err := c.contractDoc.Update(map[string]any{
		"id":                c.hashCode,
		common.KeyTimestamp: record.Timestamp,
	}); err != nil {
		return false, err
	}
	if err := c.Run(); err != nil {
		return false, err
	}
	fields, err := c.contractDoc.Fields()
	if err != nil {
		return false, err
	}
	address, _ := fields[common.KeyAddress].(string)
	pid, _ := fields[common.KeyPID].(string)
	if err := c.Connect(address, pid, fields[common.KeyProfile], fields["nodeKey"]); err != nil {
		return false, err
	}
	return true, nil
}

// Run deploys the contract, or reopens its state if it is already deployed.
// For a contract whose deployment failed it returns the stored failure.
func (c *ContractExecution) Run() error {
	fields, err := c.contractDoc.Fields()
	if err != nil {
		return err
	}
	pid, _ := fields[common.KeyPID].(string)
	timestamp, _ := fields[common.KeyTimestamp].(string)
	return c.state.Run(pid, timestamp)
}

// Connect adds a partner to the contract's partners collection.
func (c *ContractExecution) Connect(address, pid string, profile, nodeKey any) error {
	return c.partnersDB.Document(pid).Update(map[string]any{"address": address, "profile": profile,
		"nodeKey": nodeKey, "role": RolePartner})
}

// connectGuest adds a guest to the contract's partners collection.
func (c *ContractExecution) connectGuest(pid string, b *identity.JoinBody) error {
	return c.partnersDB.Document(pid).Update(map[string]any{"address": b.Node, "profile": b.Profile,
		"nodeKey": b.NodeKey.Hex(), "role": RoleGuest, "host": b.Host.Hex()})
}

// Call executes a contract method. With shouldLog the record is logged and
// executed as a write transaction; without it the call is read-only
// (eth_call) and changes nothing. A failing call returns a
// *common.ContractError, whose Error() is the reply.
func (c *ContractExecution) Call(record Record, shouldLog bool) (common.Result, error) {
	if !shouldLog {
		return c.state.View(record.Agent, record.Method, record.Message.Values, record.Timestamp)
	}
	if !c.state.Ready() {
		return common.Result{}, common.ErrNotRunning
	}
	if err := c.ledger.Log(record); err != nil {
		return common.Result{}, err
	}
	return c.state.Call(record.Agent, record.Method, record.Message.Values, record.Timestamp)
}

// Outcome is the result of applying one ledger record.
type Outcome struct {
	Index  uint64
	Record Record
	// Value is the reply: true for a deploy, the decision for a membership
	// change, the decoded return value for a write.
	Value  any
	Result common.Result
	// Err is a deterministic failure (every partner gets the same one).
	Err error
	// Decision is set for a membership change: whether it was approved.
	Decision *bool
}

// Replay applies, in order, every ledger record after the cursor, calling
// fn with each outcome. It stops at the first infrastructure error, leaving
// that record to be applied by a later Replay.
func (c *ContractExecution) Replay(fn func(Outcome)) error {
	cursor, err := c.cursor()
	if err != nil {
		return err
	}
	if cursor > 0 && !c.state.Ready() {
		if err := c.Run(); err != nil && !isDeterministic(err) {
			return err
		}
	}
	return c.ledger.Records(cursor+1, func(index uint64, record Record) error {
		out, err := c.apply(index, record)
		if err != nil {
			return fmt.Errorf("apply ledger record %d: %w", index, err)
		}
		if fn != nil {
			fn(out)
		}
		return nil
	})
}

// apply executes one logged record. It returns an error only for
// infrastructure failures; deterministic failures go in Outcome.Err.
func (c *ContractExecution) apply(index uint64, record Record) (Outcome, error) {
	out := Outcome{Index: index, Record: record}
	var err error
	switch record.Action {
	case ActionDeployContract:
		if index != 1 {
			out.Result, err = c.state.Advance()
			out.Err = fmt.Errorf("deploy_contract at ledger index %d: %w", index, ErrAlreadyCreated)
			break
		}
		out.Value, err = c.create(record)
	case ActionWrite:
		// A transaction from consensus must come from a partner or a guest.
		if known, kerr := c.partnersDB.Has(record.Agent); kerr != nil {
			return out, kerr
		} else if !known && record.Tx != nil {
			out.Result, err = c.state.Advance()
			out.Err = fmt.Errorf("%s: %w", record.Agent, ErrNotPartner)
			break
		}
		if record.Tx != nil {
			if g := record.Tx.Grant(); g != nil {
				// Signed by the agent's node: the grant must be for this code
				// and for the agent's current node here.
				if gerr := c.checkGrant(record.Agent, g); gerr != nil {
					out.Result, err = c.state.Advance()
					out.Err = gerr
					break
				}
			}
		}
		out.Result, err = c.state.Call(record.Agent, record.Method, record.Message.Values, record.Timestamp)
		out.Value = out.Result.Value
	case ActionJoin, ActionLeave, ActionRotateKey:
		var approved bool
		approved, out.Result, err = c.decide(record)
		if err == nil {
			out.Decision = &approved
			out.Value = approved
		}
	default:
		out.Result, err = c.state.Advance()
		out.Err = fmt.Errorf("unknown action %q", record.Action)
	}
	if err != nil {
		if !isDeterministic(err) {
			return out, err
		}
		out.Err = err
	}
	if out.Result == (common.Result{}) {
		out.Result = common.Result{StateRoot: c.state.Root(), BlockNumber: c.state.BlockNumber()}
	}
	return out, nil
}

// checkGrant checks a node's grant against the contract: the grant's code
// hash must be the contract's, and its node the agent's node in this
// contract (so a grant stops working when the agent moves to another node).
func (c *ContractExecution) checkGrant(agent string, g *identity.Grant) error {
	fields, err := c.contractDoc.Fields()
	if err != nil {
		return err
	}
	code, _ := fields[common.KeyCode].(string)
	if hash, err := identity.CodeHash(code); err != nil || hash != g.CodeHash {
		return fmt.Errorf("%w: it is for other code", ErrGrantDoesNotApply)
	}
	partner, err := c.partnersDB.Document(agent).Fields()
	if err != nil {
		return err
	}
	if nodeKey, _ := partner["nodeKey"].(string); !strings.EqualFold(nodeKey, g.NodeKey.Hex()) {
		return fmt.Errorf("%w: it is for another node", ErrGrantDoesNotApply)
	}
	return nil
}

// decide applies a membership change (§10 of docs/consensus.md):
//   - join: the contract's approve_partner(joiner), called as the joiner;
//     approved if the contract has no such function;
//   - a guest's join: the contract's approve_guest(joiner), called as the
//     joiner; refused if the contract has no such function;
//   - leave: always approved for oneself; removing another member calls the
//     contract's approve_removal(member) as the requester, and is refused if
//     the contract has no such function;
//   - rotate_key: approved.
//
// The decision is stored with the cursor, and the partners collection is
// updated after it.
func (c *ContractExecution) decide(record Record) (bool, common.Result, error) {
	if record.Tx == nil {
		return false, common.Result{}, fmt.Errorf("%s record without its transaction", record.Action)
	}
	body, err := record.Tx.Tx.ParseBody()
	if err != nil {
		return false, common.Result{}, err
	}
	agent := record.Tx.Tx.Agent
	id := record.HashCode
	var approved bool
	var res common.Result
	switch b := body.(type) {
	case *identity.JoinBody:
		if b.Guest {
			approved, res, err = c.state.Decide(agent.Hex(), common.MethodApproveGuest, agent, record.Timestamp, id, false)
			if err == nil && approved {
				err = c.connectGuest(agent.Hex(), b)
			}
			break
		}
		approved, res, err = c.state.Decide(agent.Hex(), common.MethodApprovePartner, agent, record.Timestamp, id, true)
		if err == nil && approved {
			err = c.Connect(b.Node, agent.Hex(), b.Profile, b.NodeKey.Hex())
		}
	case *identity.LeaveBody:
		if b.Member == agent {
			approved, res, err = c.state.Decide(agent.Hex(), "", b.Member, record.Timestamp, id, true)
		} else {
			approved, res, err = c.state.Decide(agent.Hex(), common.MethodApproveRemoval, b.Member, record.Timestamp, id, false)
		}
		if err == nil && approved {
			err = c.partnersDB.Delete(b.Member.Hex())
		}
	case *identity.RotateKeyBody:
		approved, res, err = c.state.Decide(agent.Hex(), "", agent, record.Timestamp, id, true)
		if err == nil && approved {
			err = c.partnersDB.Document(agent.Hex()).Update(map[string]any{"address": b.Node, "nodeKey": b.NodeKey.Hex()})
		}
	default:
		return false, common.Result{}, fmt.Errorf("%s record with a %T body", record.Action, body)
	}
	return approved, res, err
}

// Decision returns the stored decision on a membership change.
func Decision(doc common.Document, id string) (approved, known bool, err error) {
	fields, err := doc.Fields()
	if err != nil {
		return false, false, err
	}
	v, ok := fields[common.KeyDecisionPrefix+id]
	if !ok {
		return false, false, nil
	}
	approved, _ = v.(bool)
	return approved, true, nil
}

// cursor returns the index of the last applied ledger record.
func (c *ContractExecution) cursor() (uint64, error) {
	return readCursor(c.contractDoc)
}

// readCursor reads a contract document's cursor (its block number).
func readCursor(doc common.Document) (uint64, error) {
	fields, err := doc.Fields()
	if err != nil {
		return 0, err
	}
	switch v := fields[common.KeyBlockNumber].(type) {
	case nil:
		return 0, nil
	case uint64:
		return v, nil
	case json.Number:
		n, err := v.Int64()
		if err != nil || n < 0 {
			return 0, fmt.Errorf("contract document: invalid %s %v", common.KeyBlockNumber, v)
		}
		return uint64(n), nil
	default:
		return 0, fmt.Errorf("contract document: invalid %s %v", common.KeyBlockNumber, v)
	}
}

// isDeterministic reports whether err is an outcome every partner shares,
// as opposed to a local infrastructure failure.
func isDeterministic(err error) bool {
	var ce *common.ContractError
	return errors.As(err, &ce) || errors.Is(err, common.ErrNotImplemented)
}
