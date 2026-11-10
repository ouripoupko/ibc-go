// Package common holds the State: the per-contract EVM world state that
// replaces the Python exec-based State in ibc (common/state.py).
package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// Contract document keys.
const (
	// Written by the deployer (the deploy record's message).
	KeyPID         = "pid"         // agent id of the creator
	KeyAddress     = "address"     // network address of the creator's agent (not an EVM address)
	KeyProfile     = "profile"     // profile of the creator
	KeyCode        = "code"        // init bytecode, hex string
	KeyABI         = "abi"         // contract ABI, JSON (string or decoded array)
	KeyConstructor = "constructor" // constructor arguments, keyed by name
	KeyTimestamp   = "timestamp"   // creation timestamp (set by ContractExecution.Create)

	// Written by State.
	KeyEVMAddress    = "evm_address"    // address the contract was deployed at
	KeyStateRoot     = "state_root"     // world state root after the last write
	KeyBlockNumber   = "block_number"   // number of ledger records applied so far
	KeyLastTimestamp = "last_timestamp" // timestamp of the last write transaction
	KeyDeployError   = "deploy_error"   // why the deployment failed, if it did
	// KeyDecisionPrefix + a transaction id holds the decision on a
	// membership change, written together with the cursor.
	KeyDecisionPrefix = "decision:"
)

// Built-in method names that the Python State answered itself.
const (
	MethodGetPartners   = "get_partners"
	MethodGetParameters = "get_parameters"
	// Contract functions that decide membership changes, if a contract
	// defines them: approve_partner(address), approve_guest(address) and
	// approve_removal(address), all returning bool.
	MethodApprovePartner = "approve_partner"
	MethodApproveGuest   = "approve_guest"
	MethodApproveRemoval = "approve_removal"
)

const (
	// GasLimit bounds every execution. Gas is not paid for; it only stops
	// runaway code. It must be the same on every partner.
	GasLimit uint64 = 100_000_000
	// ChainID is returned by the CHAINID opcode. It is a fixed constant.
	ChainID = 0x1bc
	// TimestampLayout is the record timestamp format, YYYYMMDDHHMMSSffffff (UTC).
	TimestampLayout = "20060102150405"
)

var (
	// ErrNotImplemented is returned by the built-in methods that have not
	// been defined for Solidity contracts yet.
	ErrNotImplemented = errors.New("not implemented")
	// ErrNotRunning is returned when the contract has not been deployed or
	// loaded with Run.
	ErrNotRunning = errors.New("contract is not running")
	// ErrDeployFailed is the cause of the ContractError returned for every
	// record sent to a contract whose deployment failed.
	ErrDeployFailed = errors.New("contract deployment failed")
)

// ContractError is a deterministic, contract-level failure: an unknown
// method, bad arguments, a revert, or running out of gas. Every partner sees
// the same ContractError for the same record. It is the counterpart of the
// str(e) that Python returned from a failing method. Other errors from State
// are infrastructure failures (storage, commit) that are local to one agent.
type ContractError struct {
	Err    error  // underlying cause, e.g. vm.ErrExecutionReverted
	Reason string // decoded revert reason, if any
}

func (e *ContractError) Error() string {
	if e.Reason == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + ": " + e.Reason
}

func (e *ContractError) Unwrap() error { return e.Err }

// Result is the outcome of a call.
type Result struct {
	// Value is the decoded return value, JSON-serialisable.
	Value any `json:"value"`
	// StateRoot is the contract's world state root after the call. A
	// read-only or failed call leaves it unchanged.
	StateRoot ethcommon.Hash `json:"state_root"`
	// BlockNumber is the block number the call executed at.
	BlockNumber uint64 `json:"block_number"`
}

// chainConfig activates every fork up to and including Osaka (the latest one
// live on mainnet) from genesis. Amsterdam, Bogota and UBT are still under
// development in go-ethereum and stay off.
var chainConfig = func() *params.ChainConfig {
	zero := uint64(0)
	return &params.ChainConfig{
		ChainID:                 big.NewInt(ChainID),
		HomesteadBlock:          big.NewInt(0),
		EIP150Block:             big.NewInt(0),
		EIP155Block:             big.NewInt(0),
		EIP158Block:             big.NewInt(0),
		ByzantiumBlock:          big.NewInt(0),
		ConstantinopleBlock:     big.NewInt(0),
		PetersburgBlock:         big.NewInt(0),
		IstanbulBlock:           big.NewInt(0),
		MuirGlacierBlock:        big.NewInt(0),
		BerlinBlock:             big.NewInt(0),
		LondonBlock:             big.NewInt(0),
		ArrowGlacierBlock:       big.NewInt(0),
		GrayGlacierBlock:        big.NewInt(0),
		MergeNetsplitBlock:      big.NewInt(0),
		ShanghaiTime:            &zero,
		CancunTime:              &zero,
		PragueTime:              &zero,
		OsakaTime:               &zero,
		TerminalTotalDifficulty: big.NewInt(0),
		BlobScheduleConfig:      params.DefaultBlobSchedule,
	}
}()

// AgentAddress maps an agent id (pid) to its EVM address. This is how
// Python's master() becomes msg.sender.
//
// An agent id that is itself an address (the address of the agent's root
// key, as every agent has with signed transactions) is that address.
func AgentAddress(pid string) ethcommon.Address {
	if ethcommon.IsHexAddress(pid) {
		return ethcommon.HexToAddress(pid)
	}
	return ethcommon.BytesToAddress(crypto.Keccak256([]byte(pid))[12:])
}

// ParseTimestamp converts a record timestamp (YYYYMMDDHHMMSSffffff, UTC) to
// unix seconds. The microseconds are dropped.
func ParseTimestamp(ts string) (uint64, error) {
	if len(ts) < len(TimestampLayout) || len(ts) > len(TimestampLayout)+6 {
		return 0, fmt.Errorf("invalid timestamp %q: want YYYYMMDDHHMMSSffffff", ts)
	}
	for _, r := range ts {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid timestamp %q: want YYYYMMDDHHMMSSffffff", ts)
		}
	}
	t, err := time.ParseInLocation(TimestampLayout, ts[:len(TimestampLayout)], time.UTC)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q: %w", ts, err)
	}
	if t.Unix() < 0 {
		return 0, fmt.Errorf("invalid timestamp %q: before 1970", ts)
	}
	return uint64(t.Unix()), nil
}

// State is one contract instance's isolated EVM world state, the
// counterpart of the Python State. Each State owns its own trie; there is
// no state shared between contracts.
type State struct {
	mu          sync.Mutex
	contractDoc Document
	partnersDB  Collection
	trieDB      *triedb.Database
	stateDB     state.Database

	running       bool
	failed        *ContractError // set when the deployment failed; then running is false
	abi           abi.ABI
	address       ethcommon.Address
	root          ethcommon.Hash
	blockNumber   uint64
	lastTimestamp string

	// TODO: read(contract_id, method, arguments) lived here in Python: a
	// read-only call into another contract whose partners include all of
	// ours. It is dropped from the demo, as are parameters/medians,
	// random, hashcode, elapsed_time and Storage(name).
}

// NewState creates the State of one contract. db is this contract's own
// key-value store; the State keeps its trie nodes and code there. Use
// rawdb.NewMemoryDatabase for tests, or ContractDatabase over the node's
// shared Pebble database.
func NewState(contractDoc Document, partnersDB Collection, db ethdb.Database) *State {
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	return &State{
		contractDoc: contractDoc,
		partnersDB:  partnersDB,
		trieDB:      tdb,
		stateDB:     state.NewMPTDatabase(tdb, nil),
	}
}

// Run makes the contract ready for calls, like Python's run(). The first
// time it deploys the init bytecode from the contract document, with the
// constructor arguments, from the creator's address; this is a write
// transaction. Later (for example after a restart) it reopens the stored
// state root and does not run the constructor again.
//
// If the deployment fails deterministically, the failure is stored in the
// contract document and returned as a *ContractError, now and on every
// later Run; the deployment is not retried. Every record sent to such a
// contract is answered with ErrDeployFailed.
func (s *State) Run(caller, timestamp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fields, err := s.contractDoc.Fields()
	if err != nil {
		return fmt.Errorf("read contract document: %w", err)
	}
	if reason, ok := fields[KeyDeployError].(string); ok {
		blockNumber, err := toUint64(fields[KeyBlockNumber])
		if err != nil {
			return fmt.Errorf("contract document: invalid %s: %w", KeyBlockNumber, err)
		}
		s.blockNumber = blockNumber
		s.failed = &ContractError{Err: ErrDeployFailed, Reason: reason}
		return s.failed
	}
	if fields[KeyEVMAddress] != nil {
		if s.abi, err = loadABI(fields[KeyABI]); err != nil {
			return err
		}
		return s.reload(fields)
	}

	err = s.deploy(fields, caller, timestamp)
	var contractErr *ContractError
	if !errors.As(err, &contractErr) {
		return err
	}
	// A deterministic failure: the deploy record still counts as applied.
	if err := s.contractDoc.Update(map[string]any{
		KeyDeployError: contractErr.Error(),
		KeyBlockNumber: uint64(1),
	}); err != nil {
		return err
	}
	s.blockNumber = 1
	s.failed = &ContractError{Err: ErrDeployFailed, Reason: contractErr.Error()}
	return contractErr
}

// Ready reports whether Run has completed, successfully or with a stored
// deployment failure. Records may only be logged for a ready contract.
func (s *State) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running || s.failed != nil
}

func (s *State) reload(fields map[string]any) error {
	addr, ok := fields[KeyEVMAddress].(string)
	if !ok || !ethcommon.IsHexAddress(addr) {
		return fmt.Errorf("contract document: invalid %s %v", KeyEVMAddress, fields[KeyEVMAddress])
	}
	rootHex, ok := fields[KeyStateRoot].(string)
	if !ok {
		return fmt.Errorf("contract document: invalid %s %v", KeyStateRoot, fields[KeyStateRoot])
	}
	root := ethcommon.HexToHash(rootHex)
	blockNumber, err := toUint64(fields[KeyBlockNumber])
	if err != nil {
		return fmt.Errorf("contract document: invalid %s: %w", KeyBlockNumber, err)
	}
	lastTimestamp := fields[KeyLastTimestamp]

	statedb, err := state.New(root, s.stateDB)
	if err != nil {
		return fmt.Errorf("open state %s: %w", root, err)
	}
	address := ethcommon.HexToAddress(addr)
	if statedb.GetCodeSize(address) == 0 {
		if err := statedb.Error(); err != nil {
			return fmt.Errorf("open state %s: %w", root, err)
		}
		return fmt.Errorf("open state %s: no contract code at %s", root, address)
	}
	s.address = address
	s.root = root
	s.blockNumber = blockNumber
	s.lastTimestamp, _ = lastTimestamp.(string)
	s.running = true
	return nil
}

func (s *State) deploy(fields map[string]any, caller, timestamp string) error {
	contractABI, err := loadABI(fields[KeyABI])
	if err != nil {
		return err
	}
	s.abi = contractABI
	var code []byte
	switch c := fields[KeyCode].(type) {
	case string:
		var err error
		if code, err = decodeHex(c); err != nil {
			return &ContractError{Err: fmt.Errorf("invalid %s: %w", KeyCode, err)}
		}
	case []byte:
		code = c
	}
	if len(code) == 0 {
		return &ContractError{Err: fmt.Errorf("contract document has no %s", KeyCode)}
	}
	ctorValue := fields[KeyConstructor]
	ctorArgs, ok := ctorValue.(map[string]any)
	if !ok && ctorValue != nil {
		return &ContractError{Err: fmt.Errorf("%s must be an object, got %T", KeyConstructor, ctorValue)}
	}
	packed, err := packArguments(s.abi.Constructor.Inputs, ctorArgs)
	if err != nil {
		return &ContractError{Err: fmt.Errorf("constructor: %w", err)}
	}
	unixTime, err := ParseTimestamp(timestamp)
	if err != nil {
		return &ContractError{Err: err}
	}

	blockNumber := uint64(0)
	statedb, err := state.New(types.EmptyRootHash, s.stateDB)
	if err != nil {
		return err
	}
	evm, rules := newEVM(statedb, blockNumber, unixTime)
	defer evm.Release()
	from := AgentAddress(caller)
	statedb.Prepare(rules, from, ethcommon.Address{}, nil, nil, nil)
	evm.SetTxContext(vm.TxContext{Origin: from, GasPrice: new(uint256.Int)})
	ret, address, _, err := evm.Create(from, append(code, packed...), vm.NewGasBudget(GasLimit, 0), new(uint256.Int))
	if err != nil {
		return s.contractError(err, ret)
	}
	root, err := s.commit(statedb, rules, blockNumber)
	if err != nil {
		return err
	}
	if err := s.contractDoc.Update(map[string]any{
		KeyEVMAddress:    address.Hex(),
		KeyStateRoot:     root.Hex(),
		KeyBlockNumber:   blockNumber + 1,
		KeyLastTimestamp: timestamp,
	}); err != nil {
		return err
	}
	s.address = address
	s.root = root
	s.blockNumber = blockNumber + 1
	s.lastTimestamp = timestamp
	s.running = true
	return nil
}

// Call executes a write transaction from the agent caller, like Python's
// call(). method and values are ABI-encoded by name. On success the new
// state root is committed, stored in the contract document and returned.
//
// Each Call applies one logged record, so it always advances the block
// number exactly once, even when it fails: the block number is the index of
// the last applied ledger record. On failure the EVM state is rolled back
// completely and the returned error is a *ContractError carrying the decoded
// revert reason; the Result still holds the unchanged state root. Only an
// infrastructure error (storage) leaves the block number unchanged, so the
// record is applied again later.
func (s *State) Call(caller, method string, values map[string]any, timestamp string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failed != nil {
		return s.advance(s.failed)
	}
	if !s.running {
		return Result{}, ErrNotRunning
	}
	if handled, err := s.builtin(method); handled {
		return s.advance(err)
	}
	blockNumber := s.blockNumber
	result := Result{StateRoot: s.root, BlockNumber: blockNumber}

	value, statedb, rules, callErr := s.execute(caller, method, values, timestamp, blockNumber)
	var contractErr *ContractError
	if callErr != nil && !errors.As(callErr, &contractErr) {
		return result, callErr // infrastructure failure: nothing is recorded
	}
	if callErr != nil {
		return s.advance(callErr)
	}
	root, err := s.commit(statedb, rules, blockNumber)
	if err != nil {
		return result, err
	}
	if err := s.contractDoc.Update(map[string]any{
		KeyBlockNumber:   blockNumber + 1,
		KeyStateRoot:     root.Hex(),
		KeyLastTimestamp: timestamp,
	}); err != nil {
		return result, err
	}
	s.blockNumber = blockNumber + 1
	s.root = root
	s.lastTimestamp = timestamp
	result.StateRoot = root
	result.Value = value
	return result, nil
}

// Advance applies a logged record that executes no transaction (for
// example a join from an existing partner): it only advances the block
// number.
func (s *State) Advance() (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running && s.failed == nil {
		return Result{}, ErrNotRunning
	}
	return s.advance(nil)
}

// advance moves the block number forward without changing the state, and
// returns callErr with the unchanged root. extra fields are written to the
// contract document in the same update.
func (s *State) advance(callErr error, extra ...map[string]any) (Result, error) {
	result := Result{StateRoot: s.root, BlockNumber: s.blockNumber}
	update := map[string]any{KeyBlockNumber: s.blockNumber + 1}
	for _, e := range extra {
		maps.Copy(update, e)
	}
	if err := s.contractDoc.Update(update); err != nil {
		return result, err
	}
	s.blockNumber++
	return result, callErr
}

// Decide applies the record of a membership change. If the contract defines
// method (approve_partner or approve_removal) as a function of one address
// returning bool, it is called as caller with subject, and its answer is the
// decision; a revert means no. Otherwise the decision is def. The decision
// is stored in the contract document under KeyDecisionPrefix+id, in the same
// update that advances the cursor, so a crash cannot lose it.
func (s *State) Decide(caller, method string, subject ethcommon.Address, timestamp, id string, def bool) (bool, Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := KeyDecisionPrefix + id
	if s.failed != nil {
		r, _ := s.advance(nil, map[string]any{key: false})
		return false, r, nil
	}
	if !s.running {
		return false, Result{}, ErrNotRunning
	}
	m, ok := s.abi.Methods[method]
	if !ok || len(m.Inputs) != 1 || m.Inputs[0].Type.T != abi.AddressTy ||
		len(m.Outputs) != 1 || m.Outputs[0].Type.T != abi.BoolTy {
		r, err := s.advance(nil, map[string]any{key: def})
		return def, r, err
	}
	blockNumber := s.blockNumber
	result := Result{StateRoot: s.root, BlockNumber: blockNumber}
	values := map[string]any{argumentKey(m.Inputs[0], 0): subject.Hex()}
	value, statedb, rules, callErr := s.execute(caller, method, values, timestamp, blockNumber)
	var contractErr *ContractError
	if callErr != nil && !errors.As(callErr, &contractErr) {
		return false, result, callErr
	}
	if callErr != nil {
		r, err := s.advance(nil, map[string]any{key: false})
		return false, r, err
	}
	approved, _ := value.(bool)
	root, err := s.commit(statedb, rules, blockNumber)
	if err != nil {
		return false, result, err
	}
	if err := s.contractDoc.Update(map[string]any{
		KeyBlockNumber:   blockNumber + 1,
		KeyStateRoot:     root.Hex(),
		KeyLastTimestamp: timestamp,
		key:              approved,
	}); err != nil {
		return false, result, err
	}
	s.blockNumber = blockNumber + 1
	s.root = root
	s.lastTimestamp = timestamp
	result.StateRoot = root
	result.Value = approved
	return approved, result, nil
}

// View executes a read-only call, the equivalent of eth_call: it runs on a
// fresh copy of the current state that is then discarded, so neither the
// state, the root nor the contract document changes. It serves Python's
// call with should_log=False and dispatcher/contract_view.py. If timestamp
// is empty, the timestamp of the last write is used.
func (s *State) View(caller, method string, values map[string]any, timestamp string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := Result{StateRoot: s.root, BlockNumber: s.blockNumber}
	if s.failed != nil {
		return result, s.failed
	}
	if !s.running {
		return Result{}, ErrNotRunning
	}
	if handled, err := s.builtin(method); handled {
		return result, err
	}
	// A read runs only view and pure functions: a function that may change
	// the state is a transaction, ordered by consensus.
	if m, ok := s.abi.Methods[method]; ok && !m.IsConstant() {
		return result, &ContractError{Err: fmt.Errorf("%s changes state: send it as a transaction (contract_write)", method)}
	}
	if timestamp == "" {
		timestamp = s.lastTimestamp
	}
	value, _, _, err := s.execute(caller, method, values, timestamp, s.blockNumber)
	result.Value = value
	return result, err
}

// Root returns the current state root.
func (s *State) Root() ethcommon.Hash {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root
}

// Address returns the contract's EVM address.
func (s *State) Address() ethcommon.Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.address
}

// BlockNumber returns the number of write transactions executed so far.
func (s *State) BlockNumber() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blockNumber
}

// execute runs method on a fresh StateDB opened at the current root. The
// caller decides whether to commit the returned StateDB.
func (s *State) execute(caller, method string, values map[string]any, timestamp string, blockNumber uint64) (any, *state.StateDB, params.Rules, error) {
	m, ok := s.abi.Methods[method]
	if !ok {
		return nil, nil, params.Rules{}, &ContractError{Err: fmt.Errorf("unknown method %q", method)}
	}
	args, err := packArguments(m.Inputs, values)
	if err != nil {
		return nil, nil, params.Rules{}, &ContractError{Err: fmt.Errorf("%s: %w", method, err)}
	}
	unixTime, err := ParseTimestamp(timestamp)
	if err != nil {
		return nil, nil, params.Rules{}, &ContractError{Err: err}
	}
	statedb, err := state.New(s.root, s.stateDB)
	if err != nil {
		return nil, nil, params.Rules{}, err
	}
	evm, rules := newEVM(statedb, blockNumber, unixTime)
	defer evm.Release()
	from := AgentAddress(caller)
	statedb.Prepare(rules, from, ethcommon.Address{}, &s.address, nil, nil)
	evm.SetTxContext(vm.TxContext{Origin: from, GasPrice: new(uint256.Int)})
	ret, _, err := evm.Call(from, s.address, append(slices.Clone(m.ID), args...), vm.NewGasBudget(GasLimit, 0), new(uint256.Int))
	if err != nil {
		return nil, nil, rules, s.contractError(err, ret)
	}
	if err := statedb.Error(); err != nil {
		return nil, nil, rules, err
	}
	value, err := unpackOutputs(m.Outputs, ret)
	if err != nil {
		return nil, nil, rules, &ContractError{Err: fmt.Errorf("%s: decode output: %w", method, err)}
	}
	return value, statedb, rules, nil
}

// commit writes the StateDB's changes and flushes the trie nodes to the
// key-value store, so the root can be reopened after a restart.
func (s *State) commit(statedb *state.StateDB, rules params.Rules, blockNumber uint64) (ethcommon.Hash, error) {
	root, err := statedb.Commit(rules, blockNumber)
	if err != nil {
		return ethcommon.Hash{}, fmt.Errorf("commit state: %w", err)
	}
	if err := s.trieDB.Commit(root, false); err != nil {
		return ethcommon.Hash{}, fmt.Errorf("flush state %s: %w", root, err)
	}
	return root, nil
}

// contractError builds a ContractError from an EVM error, decoding the revert
// reason from the return data: Error(string), Panic(uint256), or a custom
// error declared in the contract's ABI.
func (s *State) contractError(err error, ret []byte) *ContractError {
	ce := &ContractError{Err: err}
	if !errors.Is(err, vm.ErrExecutionReverted) || len(ret) == 0 {
		return ce
	}
	if reason, uerr := abi.UnpackRevert(ret); uerr == nil {
		ce.Reason = reason
		return ce
	}
	if len(ret) >= 4 {
		for _, e := range s.abi.Errors {
			if !bytes.Equal(ret[:4], e.ID[:4]) {
				continue
			}
			if values, uerr := e.Inputs.Unpack(ret[4:]); uerr == nil {
				args := make([]string, len(values))
				for i, v := range values {
					b, _ := json.Marshal(toJSON(e.Inputs[i].Type, reflect.ValueOf(v)))
					args[i] = string(b)
				}
				ce.Reason = e.Name + "(" + strings.Join(args, ", ") + ")"
				return ce
			}
		}
	}
	ce.Reason = hexutil.Encode(ret)
	return ce
}

// builtin handles the method names that Python's State answered itself.
func (s *State) builtin(method string) (bool, error) {
	switch method {
	case MethodGetPartners:
		return true, s.getPartners()
	case MethodGetParameters:
		return true, s.getParameters()
	}
	return false, nil
}

// TODO: define get_partners for Solidity contracts. In Python it returned
// the partners collection (address, profile, agent) unless the contract
// class overrode it.
func (s *State) getPartners() error {
	return fmt.Errorf("%s: %w", MethodGetPartners, ErrNotImplemented)
}

// TODO: define get_parameters for Solidity contracts. In Python it returned
// the caller's submitted parameters and the consensus medians.
func (s *State) getParameters() error {
	return fmt.Errorf("%s: %w", MethodGetParameters, ErrNotImplemented)
}

func loadABI(v any) (abi.ABI, error) {
	if v == nil {
		return abi.ABI{}, &ContractError{Err: fmt.Errorf("contract document has no %s", KeyABI)}
	}
	var text string
	switch a := v.(type) {
	case string:
		text = a
	case []byte:
		text = string(a)
	default:
		b, err := json.Marshal(a)
		if err != nil {
			return abi.ABI{}, &ContractError{Err: fmt.Errorf("invalid %s: %w", KeyABI, err)}
		}
		text = string(b)
	}
	parsed, err := abi.JSON(strings.NewReader(text))
	if err != nil {
		return abi.ABI{}, &ContractError{Err: fmt.Errorf("invalid %s: %w", KeyABI, err)}
	}
	return parsed, nil
}

// newEVM builds an EVM whose block context depends only on the record
// (timestamp) and the contract's history (block number), never on the
// local clock, so every partner computes the same result.
func newEVM(statedb *state.StateDB, blockNumber, unixTime uint64) (*vm.EVM, params.Rules) {
	number := new(big.Int).SetUint64(blockNumber)
	random := ethcommon.Hash{}
	blockCtx := vm.BlockContext{
		CanTransfer: canTransfer,
		Transfer:    transfer,
		GetHash:     func(uint64) ethcommon.Hash { return ethcommon.Hash{} },
		Coinbase:    ethcommon.Address{},
		GasLimit:    GasLimit,
		BlockNumber: number,
		Time:        unixTime,
		Difficulty:  big.NewInt(0),
		BaseFee:     big.NewInt(0),
		BlobBaseFee: big.NewInt(0),
		Random:      &random,
	}
	evm := vm.NewEVM(blockCtx, statedb, chainConfig, vm.Config{})
	// No precompiles at all.
	evm.SetPrecompiles(vm.PrecompiledContracts{})
	return evm, chainConfig.Rules(number, true, unixTime)
}

// canTransfer and transfer match core.CanTransfer and core.Transfer. All
// balances are zero and calls carry no value, so in practice only zero
// transfers succeed.
func canTransfer(db vm.StateDB, addr ethcommon.Address, amount *uint256.Int) bool {
	return db.GetBalance(addr).Cmp(amount) >= 0
}

func transfer(db vm.StateDB, sender, recipient ethcommon.Address, amount *uint256.Int, _ *params.Rules) {
	db.SubBalance(sender, amount, tracing.BalanceChangeTransfer)
	db.AddBalance(recipient, amount, tracing.BalanceChangeTransfer)
}

func toUint64(v any) (uint64, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case float64:
		if n < 0 || n != math.Trunc(n) || n > 1<<53 {
			return 0, fmt.Errorf("not a block number: %v", n)
		}
		return uint64(n), nil
	case string:
		return strconv.ParseUint(n, 10, 64)
	}
	b, err := toBigInt(v)
	if err != nil {
		return 0, err
	}
	if !b.IsUint64() {
		return 0, fmt.Errorf("not a block number: %s", b)
	}
	return b.Uint64(), nil
}
