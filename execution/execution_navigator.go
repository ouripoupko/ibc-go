package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/common"
)

// Collection names, as in the Python MongoDB layout:
// agents/<agent> {address} / contracts/<hash> {contract document} / pda_partners/<pid>.
const (
	AgentsCollection    = "agents"
	ContractsCollection = "contracts"
)

var (
	ErrUnknownAgent    = errors.New("unknown agent")
	ErrUnknownContract = errors.New("unknown contract")
	ErrUnknownAction   = errors.New("unknown action")
	ErrClosed          = errors.New("navigator is closed")
)

// Reply is what the Navigator publishes for every applied record, the
// message Python published on the agent's Redis channel.
type Reply struct {
	Request  string  `json:"request"`  // the record's hash_code
	Contract *string `json:"contract"` // null for agent-level actions
	Action   string  `json:"action"`
	// Reply is the result, or the error string for a failed record (as
	// Python returned str(e)).
	Reply any `json:"reply"`
	// StateRoot is the contract's state root after the record.
	StateRoot string `json:"state_root,omitempty"`
}

// Output receives what execution produces for the rest of the node.
type Output interface {
	// Publish delivers a reply to the agent's clients (the SSE stream).
	Publish(agent string, reply Reply)
	// Decided reports to consensus how a membership change was decided.
	Decided(agent, contract string, record Record, approved bool)
	// Applied reports to consensus that the record at index was applied,
	// and the contract's state root after it.
	Applied(agent, contract string, index uint64, stateRoot string)
}

// Config configures a Navigator.
type Config struct {
	DB     ethdb.Database // the node's database
	Output Output         // may be nil
	// IdleTimeout closes a contract's goroutine after this long without
	// records. Default 60 seconds, as in Python.
	IdleTimeout time.Duration
	// RetryDelay is the wait before applying a record again after an
	// infrastructure error. Default 5 seconds.
	RetryDelay time.Duration
}

// Navigator executes committed records: the counterpart of Python's
// execution_service.py and ExecutionNavigator, without Redis.
//
// Submit appends a record to its contract's ledger, which makes it
// durable, and wakes the contract's goroutine. That goroutine applies every
// ledger record after the contract's cursor (ContractExecution.Replay), in
// order. Each contract has its own goroutine, created on demand and closed
// when idle; contracts never wait for each other. After a restart, Recover
// wakes every contract whose ledger is ahead of its cursor.
type Navigator struct {
	cfg    Config
	agents common.Collection
	quit   chan struct{}
	wg     sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	chains  map[string]*common.BlockChain
	workers map[workerKey]*worker
}

type workerKey struct{ agent, contract string }

type worker struct {
	wake chan struct{} // capacity 1: "there may be new records"
}

// NewNavigator returns a Navigator.
func NewNavigator(cfg Config) *Navigator {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 5 * time.Second
	}
	if cfg.Output == nil {
		cfg.Output = discardOutput{}
	}
	return &Navigator{
		cfg:     cfg,
		agents:  common.NewKVStore(cfg.DB).Collection(AgentsCollection),
		quit:    make(chan struct{}),
		chains:  map[string]*common.BlockChain{},
		workers: map[workerKey]*worker{},
	}
}

// Submit accepts a record that consensus has committed for agent. Once
// Submit returns nil the record is in the ledger and will be applied, even
// across a crash. A record whose Index is already in the ledger is ignored,
// so redelivery is safe. Errors mean the record was not accepted.
func (n *Navigator) Submit(agent string, record Record) error {
	if n.isClosed() {
		return ErrClosed
	}
	switch record.Action {
	case ActionRegisterAgent:
		return n.registerAgent(agent, record)
	case ActionDeployContract:
		if record.Contract == "" {
			record.Contract = record.HashCode // as Python's dispatcher sets it
		}
		if err := n.storeContract(agent, record); err != nil {
			return err
		}
	case ActionWrite, ActionJoin, ActionLeave, ActionRotateKey:
		exists, err := n.contracts(agent).Has(record.Contract)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w %q of agent %q", ErrUnknownContract, record.Contract, agent)
		}
	default:
		return fmt.Errorf("%w %q", ErrUnknownAction, record.Action)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, appended, err := n.ledger(agent, record.Contract).AppendAt(record.Index, raw)
	if err != nil || !appended {
		return err
	}
	n.wake(agent, record.Contract)
	return nil
}

// registerAgent stores the agent's node address and node key, from the
// agent's signed registration (checked by the dispatcher).
func (n *Navigator) registerAgent(agent string, record Record) error {
	fields := map[string]any{}
	for _, k := range AgentFields {
		if v, ok := record.Message.Fields[k].(string); ok {
			fields[k] = v
		}
	}
	if err := n.agents.Document(agent).Update(fields); err != nil {
		return err
	}
	n.cfg.Output.Publish(agent, Reply{Request: record.HashCode, Action: record.Action, Reply: true})
	return nil
}

// storeContract stores a deploy record's message as the contract document
// (Python: contracts_db[hash_code] = record['message']).
func (n *Navigator) storeContract(agent string, record Record) error {
	registered, err := n.agents.Has(agent)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("%w %q", ErrUnknownAgent, agent)
	}
	if record.Contract == "" {
		return fmt.Errorf("deploy_contract: no contract id")
	}
	length, err := n.ledger(agent, record.Contract).Len()
	if err != nil || length > 0 {
		return err // already deployed: the ledger decides what happens to the record
	}
	if len(record.Message.Fields) == 0 {
		return fmt.Errorf("deploy_contract: empty message")
	}
	return n.contracts(agent).Document(record.Contract).Update(record.Message.Fields)
}

// Recover wakes every contract whose ledger has records that were not
// applied yet, for example after a crash. Call it once at startup.
func (n *Navigator) Recover() error {
	agents, err := n.agents.Keys()
	if err != nil {
		return err
	}
	for _, agent := range agents {
		contracts := n.contracts(agent)
		hashes, err := contracts.Keys()
		if err != nil {
			return err
		}
		for _, hash := range hashes {
			cursor, err := readCursor(contracts.Document(hash))
			if err != nil {
				return fmt.Errorf("contract %q of agent %q: %w", hash, agent, err)
			}
			length, err := n.ledger(agent, hash).Len()
			if err != nil {
				return err
			}
			if length > cursor {
				n.wake(agent, hash)
			}
		}
	}
	return nil
}

// Close applies what is already in the ledgers, then stops every goroutine.
func (n *Navigator) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	close(n.quit)
	n.mu.Unlock()
	n.wg.Wait()
}

func (n *Navigator) isClosed() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.closed
}

func (n *Navigator) contracts(agent string) common.Collection {
	return n.agents.Document(agent).GetSubCollection(ContractsCollection)
}

func (n *Navigator) ledger(agent, contract string) *common.ContractLedger {
	n.mu.Lock()
	defer n.mu.Unlock()
	chain, ok := n.chains[agent]
	if !ok {
		chain = common.NewBlockChain(n.cfg.DB, agent)
		n.chains[agent] = chain
	}
	return chain.Contract(contract)
}

// wake signals the contract's goroutine, starting it if needed.
func (n *Navigator) wake(agent, contract string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return // the record is in the ledger; Recover applies it on the next start
	}
	key := workerKey{agent, contract}
	w, ok := n.workers[key]
	if !ok {
		w = &worker{wake: make(chan struct{}, 1)}
		n.workers[key] = w
		n.wg.Add(1)
		go n.run(key, w)
	}
	select {
	case w.wake <- struct{}{}:
	default: // already signalled
	}
}

// run is a contract's goroutine.
func (n *Navigator) run(key workerKey, w *worker) {
	defer n.wg.Done()
	exec, err := n.open(key)
	if err != nil {
		slog.Error("open contract", "agent", key.agent, "contract", key.contract, "err", err)
		n.mu.Lock()
		delete(n.workers, key) // the next Submit or Recover tries again
		n.mu.Unlock()
		return
	}
	idle := time.NewTimer(n.cfg.IdleTimeout)
	defer idle.Stop()
	var retry <-chan time.Time
	for {
		select {
		case <-w.wake:
		case <-retry:
		case <-n.quit:
			n.replay(key, exec) // apply what is already logged before stopping
			return
		case <-idle.C:
			n.mu.Lock()
			if len(w.wake) == 0 && retry == nil {
				delete(n.workers, key)
				n.mu.Unlock()
				return
			}
			n.mu.Unlock()
		}
		retry = nil
		if err := n.replay(key, exec); err != nil {
			slog.Error("apply records", "agent", key.agent, "contract", key.contract, "err", err)
			retry = time.After(n.cfg.RetryDelay)
		}
		idle.Reset(n.cfg.IdleTimeout)
	}
}

func (n *Navigator) open(key workerKey) (*ContractExecution, error) {
	doc := n.contracts(key.agent).Document(key.contract)
	ledger := ChainLedger{n.ledger(key.agent, key.contract)}
	return NewContractExecution(doc, key.contract, key.agent, n.cfg.DB, ledger), nil
}

func (n *Navigator) replay(key workerKey, exec *ContractExecution) error {
	return exec.Replay(func(out Outcome) { n.report(key.agent, out) })
}

// report publishes an outcome, and tells consensus about decisions and progress.
func (n *Navigator) report(agent string, out Outcome) {
	contract := out.Record.Contract
	reply := Reply{
		Request:   out.Record.HashCode,
		Contract:  &contract,
		Action:    out.Record.Action,
		Reply:     out.Value,
		StateRoot: out.Result.StateRoot.Hex(),
	}
	if out.Err != nil {
		reply.Reply = out.Err.Error()
	}
	if out.Decision != nil {
		n.cfg.Output.Decided(agent, contract, out.Record, *out.Decision)
	}
	n.cfg.Output.Applied(agent, contract, out.Index, out.Result.StateRoot.Hex())
	n.cfg.Output.Publish(agent, reply)
}

// Progress returns the index of the last record applied in agent's
// contract, and the state root after it.
func (n *Navigator) Progress(agent, contract string) (uint64, string, error) {
	doc := n.contracts(agent).Document(contract)
	cursor, err := readCursor(doc)
	if err != nil {
		return 0, "", err
	}
	fields, err := doc.Fields()
	if err != nil {
		return 0, "", err
	}
	root, _ := fields[common.KeyStateRoot].(string)
	return cursor, root, nil
}

// Decision returns how a membership change of agent's contract was decided,
// and false if it has not been applied yet.
func (n *Navigator) Decision(agent, contract, id string) (approved, known bool, err error) {
	return Decision(n.contracts(agent).Document(contract), id)
}

// activeWorkers returns the number of contract goroutines (for tests).
func (n *Navigator) activeWorkers() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.workers)
}

type discardOutput struct{}

func (discardOutput) Publish(string, Reply)                  {}
func (discardOutput) Decided(string, string, Record, bool)   {}
func (discardOutput) Applied(string, string, uint64, string) {}
