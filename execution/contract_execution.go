// Package execution holds ContractExecution, the counterpart of the Python
// ContractExecution in ibc (execution/contract_execution.py), and the
// Navigator that feeds it, the counterpart of execution_navigator.py and
// execution_service.py.
package execution

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/common"
)

// PartnersCollection is the name of the contract document's partners
// sub-collection.
const PartnersCollection = "pda_partners"

// ErrAlreadyCreated is returned by Create for a contract that already has
// its deploy record applied.
var ErrAlreadyCreated = errors.New("contract already created")

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
// record applied. Every record that is logged is applied exactly once and
// advances it by one, whatever the outcome, so Replay knows where to resume
// after a restart.
type ContractExecution struct {
	contractDoc common.Document
	hashCode    string
	me          string
	myAddress   string
	ledger      Ledger
	partnersDB  common.Collection
	state       *common.State
	outbox      *common.Outbox
}

// NewContractExecution creates the execution of the contract hashCode,
// owned by agent me (reachable at myAddress). Python passed the navigator
// to the State only for read(), which is dropped; db takes its place: the
// node's database, in which the contract's EVM state gets its own key range
// (common.ContractDatabase). sender delivers the join replies to partners.
func NewContractExecution(contractDoc common.Document, hashCode, me, myAddress string,
	db ethdb.Database, ledger Ledger, sender common.Sender) *ContractExecution {
	partnersDB := contractDoc.GetSubCollection(PartnersCollection)
	return &ContractExecution{
		contractDoc: contractDoc,
		hashCode:    hashCode,
		me:          me,
		myAddress:   myAddress,
		ledger:      ledger,
		partnersDB:  partnersDB,
		state:       common.NewState(contractDoc, partnersDB, common.ContractDatabase(db, me, hashCode)),
		outbox:      common.NewOutbox(sender),
	}
}

// Close stops the outbox after it has sent every queued reply.
func (c *ContractExecution) Close() {
	c.outbox.Close()
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
	if err := c.Connect(address, pid, fields[common.KeyProfile]); err != nil {
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

// Join logs and applies a partner's request to join the contract
// (a2a_connect) and returns whether it was approved. If this agent is the
// addressee, the decision is sent back to the requesting agent.
func (c *ContractExecution) Join(record Record) (bool, error) {
	if !c.state.Ready() {
		return false, common.ErrNotRunning
	}
	if err := c.ledger.Log(record); err != nil {
		return false, err
	}
	return c.join(record)
}

func (c *ContractExecution) join(record Record) (bool, error) {
	message := record.Message
	msg := message.Msg
	if msg == nil {
		msg = &JoinMsg{}
	}
	approve := false
	isPartner, err := c.partnersDB.Has(msg.PID)
	if err != nil {
		return false, err
	}
	if isPartner {
		if _, err := c.state.Advance(); err != nil {
			return false, err
		}
	} else {
		// TODO: approve_partner is a stub until its semantics are defined for
		// Solidity contracts. It returns ErrNotImplemented, which is treated
		// as "not approved" for now. In Python it returned True by default.
		_, err := c.state.Call(message.To, common.MethodApprovePartner,
			map[string]any{"partner": msg.PID}, record.Timestamp)
		if err != nil && !isDeterministic(err) {
			return false, err
		}
	}
	// TODO: the partners collection is not part of the state root, so a crash
	// between Connect and the block number update above can make a replayed
	// join see the partner as existing. Once approve_partner is defined, keep
	// the partner list in the contract's EVM state.
	if approve {
		if err := c.Connect(msg.Address, msg.PID, msg.Profile); err != nil {
			return false, err
		}
	}
	if message.To == c.me {
		partner := common.NewPartner(msg.Address, msg.PID, c.myAddress, c.me, nil, c.outbox)
		if err := partner.ReplyJoin(c.hashCode, approve); err != nil {
			return approve, err
		}
	}
	return approve, nil
}

// Connect adds a partner to the contract.
func (c *ContractExecution) Connect(address, pid string, profile any) error {
	return c.partnersDB.Document(pid).Update(map[string]any{"address": address, "profile": profile})
}

// Call executes a contract method. With shouldLog the record is logged and
// executed as a write transaction; without it the call is read-only
// (eth_call) and changes nothing. A failing call returns a *common.ContractError,
// whose Error() is the reply Python produced with str(e).
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
	// Value is the reply: true for a deploy, the approval for a join, the
	// decoded return value for a write.
	Value  any
	Result common.Result
	// Err is a deterministic failure (every partner gets the same one).
	Err error
}

// Replay applies, in order, every ledger record after the cursor, calling
// fn with each outcome. It is the normal way records are executed by the
// Navigator, and also recovers after a crash. It stops at the first
// infrastructure error, leaving that record to be applied by a later Replay.
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
	case ActionConnect:
		if !c.state.Ready() {
			return out, common.ErrNotRunning
		}
		out.Value, err = c.join(record)
	case ActionWrite:
		out.Result, err = c.state.Call(record.Agent, record.Method, record.Message.Values, record.Timestamp)
		out.Value = out.Result.Value
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
