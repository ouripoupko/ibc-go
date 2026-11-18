// Package common holds the State: the per-contract EVM world state that
// replaces the Python exec-based State in ibc (common/state.py).
package common

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
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
	KeyPID        = "pid"        // agent id of the creator
	KeyAddress    = "address"    // network address of the creator's agent (not an EVM address)
	KeyProfile    = "profile"    // profile of the creator
	KeyCode       = "code"       // the root contract's creation bytecode, hex string
	KeyArgs       = "args"       // its ABI-encoded constructor arguments, hex string
	KeyABIs       = "abis"       // the ABIs of the source's contracts, by name (for users' nodes; execution does not read them)
	KeyMembership = "membership" // the membership functions the root contract defines
	KeyTimestamp  = "timestamp"  // creation timestamp (set by ContractExecution.Create)

	// Written by State.
	KeyEVMAddress    = "evm_address"    // address the root contract was deployed at
	KeyStateRoot     = "state_root"     // world state root after the last write
	KeyBlockNumber   = "block_number"   // number of ledger records applied so far
	KeyLastTimestamp = "last_timestamp" // timestamp of the last write transaction
	KeyDeployError   = "deploy_error"   // why the deployment failed, if it did
	// KeyDecisionPrefix + a transaction id holds the decision on a
	// membership change, written together with the cursor.
	KeyDecisionPrefix = "decision:"
)

const (
	// GasLimit bounds every execution. Gas is not paid for; it only stops
	// runaway code. It must be the same on every partner.
	GasLimit uint64 = 100_000_000
	// PublicGasLimit bounds an anonymous read (public_read): anyone may send
	// one, so it gets less than a partner's call.
	PublicGasLimit uint64 = 10_000_000
	// ChainID is returned by the CHAINID opcode. It is a fixed constant.
	ChainID = 0x1bc
	// TimestampLayout is the record timestamp format, YYYYMMDDHHMMSSffffff (UTC).
	TimestampLayout = "20060102150405"
)

var (
	// ErrNotRunning is returned when the contract has not been deployed or
	// loaded with Run.
	ErrNotRunning = errors.New("contract is not running")
	// ErrDeployFailed is the cause of the ContractError returned for every
	// record sent to a contract whose deployment failed.
	ErrDeployFailed = errors.New("contract deployment failed")
)

// ContractError is a deterministic, contract-level failure: a revert, or
// running out of gas. Every partner sees the same ContractError for the same
// record. Other errors from State are infrastructure failures (storage,
// commit) that are local to one agent.
type ContractError struct {
	Err    error  // underlying cause, e.g. vm.ErrExecutionReverted
	Reason string // the revert reason: Error(string)'s or Panic's, or else the revert data as hex
	Data   []byte // the revert data, if any (a user's node decodes custom errors with the source's ABIs)
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
	// Value is what the call returned: for a call, its return data as hex
	// (a user's node decodes it with the source's ABIs); for a membership
	// decision, the decision.
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
	membership    []string       // the membership functions the root contract defines
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
	if s.membership, err = membershipOf(fields[KeyMembership]); err != nil {
		return err
	}
	if fields[KeyEVMAddress] != nil {
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
	code, err := hexField(fields, KeyCode)
	if err != nil {
		return err
	}
	if len(code) == 0 {
		return &ContractError{Err: fmt.Errorf("contract document has no %s", KeyCode)}
	}
	args, err := hexField(fields, KeyArgs)
	if err != nil {
		return err
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
	statedb.Prepare(rules, from, ethcommon.Address{}, nil, vm.ActivePrecompiles(rules), nil)
	evm.SetTxContext(vm.TxContext{Origin: from, GasPrice: new(uint256.Int)})
	ret, address, _, err := evm.Create(from, append(code, args...), vm.NewGasBudget(GasLimit, 0), new(uint256.Int))
	if err != nil {
		return contractError(err, ret)
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

// Call executes a write transaction from the agent caller: data (calldata)
// on the contract at to, or on the root contract if to is nil. On success
// the new state root is committed, stored in the contract document and
// returned, with the return data (hex) as the value.
//
// Each Call applies one logged record, so it always advances the block
// number exactly once, even when it fails: the block number is the index of
// the last applied ledger record. On failure the EVM state is rolled back
// completely and the returned error is a *ContractError carrying the revert
// reason; the Result still holds the unchanged state root. Only an
// infrastructure error (storage) leaves the block number unchanged, so the
// record is applied again later.
func (s *State) Call(caller string, to *ethcommon.Address, data []byte, timestamp string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failed != nil {
		return s.advance(s.failed)
	}
	if !s.running {
		return Result{}, ErrNotRunning
	}
	blockNumber := s.blockNumber
	result := Result{StateRoot: s.root, BlockNumber: blockNumber}

	ret, statedb, rules, callErr := s.execute(caller, to, data, timestamp, blockNumber, GasLimit)
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
	result.Value = hexutil.Encode(ret)
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

// Decide applies the record of a membership change. If the root contract
// defines function (a membership function: approve_partner, approve_guest or
// approve_removal), it is called as caller with subject, and decides: true
// approves, anything else (false, a revert) refuses. Otherwise the decision
// is def. An empty function also means def. The decision is stored in the
// contract document under KeyDecisionPrefix+id, in the same update that
// advances the cursor, so a crash cannot lose it.
func (s *State) Decide(caller, function string, subject ethcommon.Address, timestamp, id string, def bool) (bool, Result, error) {
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
	if function == "" || !slices.Contains(s.membership, function) {
		r, err := s.advance(nil, map[string]any{key: def})
		return def, r, err
	}
	blockNumber := s.blockNumber
	result := Result{StateRoot: s.root, BlockNumber: blockNumber}
	data := append(crypto.Keccak256([]byte(function + "(address)"))[:4], ethcommon.LeftPadBytes(subject.Bytes(), 32)...)
	ret, statedb, rules, callErr := s.execute(caller, nil, data, timestamp, blockNumber, GasLimit)
	var contractErr *ContractError
	if callErr != nil && !errors.As(callErr, &contractErr) {
		return false, result, callErr
	}
	if callErr != nil {
		r, err := s.advance(nil, map[string]any{key: false})
		return false, r, err
	}
	// The ABI encoding of true: one word, 1.
	approved := len(ret) == 32 && new(big.Int).SetBytes(ret).Cmp(big.NewInt(1)) == 0
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

// View executes a read-only call, the equivalent of eth_call: data on the
// contract at to (nil: the root contract), on a fresh copy of the current
// state that is then discarded, so neither the state, the root nor the
// contract document changes. The value is the return data, as hex. If
// timestamp is empty, the timestamp of the last write is used.
func (s *State) View(caller string, to *ethcommon.Address, data []byte, timestamp string) (Result, error) {
	return s.ViewGas(caller, to, data, timestamp, GasLimit)
}

// ViewGas is View with a gas limit.
func (s *State) ViewGas(caller string, to *ethcommon.Address, data []byte, timestamp string, gas uint64) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := Result{StateRoot: s.root, BlockNumber: s.blockNumber}
	if s.failed != nil {
		return result, s.failed
	}
	if !s.running {
		return Result{}, ErrNotRunning
	}
	if timestamp == "" {
		timestamp = s.lastTimestamp
	}
	ret, _, _, err := s.execute(caller, to, data, timestamp, s.blockNumber, gas)
	if err == nil {
		result.Value = hexutil.Encode(ret)
	}
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

// execute runs data on the contract at to (nil: the root contract), on a
// fresh StateDB opened at the current root. The caller decides whether to
// commit the returned StateDB.
func (s *State) execute(caller string, to *ethcommon.Address, data []byte, timestamp string, blockNumber, gas uint64) ([]byte, *state.StateDB, params.Rules, error) {
	unixTime, err := ParseTimestamp(timestamp)
	if err != nil {
		return nil, nil, params.Rules{}, &ContractError{Err: err}
	}
	target := s.address
	if to != nil {
		target = *to
	}
	statedb, err := state.New(s.root, s.stateDB)
	if err != nil {
		return nil, nil, params.Rules{}, err
	}
	evm, rules := newEVM(statedb, blockNumber, unixTime)
	defer evm.Release()
	from := AgentAddress(caller)
	statedb.Prepare(rules, from, ethcommon.Address{}, &target, vm.ActivePrecompiles(rules), nil)
	evm.SetTxContext(vm.TxContext{Origin: from, GasPrice: new(uint256.Int)})
	ret, _, err := evm.Call(from, target, slices.Clone(data), vm.NewGasBudget(gas, 0), new(uint256.Int))
	if err != nil {
		return nil, nil, rules, contractError(err, ret)
	}
	if err := statedb.Error(); err != nil {
		return nil, nil, rules, err
	}
	return ret, statedb, rules, nil
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

// contractError builds a ContractError from an EVM error: the reason of a
// revert with Error(string) or Panic(uint256), or else its data as hex.
func contractError(err error, ret []byte) *ContractError {
	ce := &ContractError{Err: err}
	if !errors.Is(err, vm.ErrExecutionReverted) || len(ret) == 0 {
		return ce
	}
	ce.Data = slices.Clone(ret)
	if reason, uerr := abi.UnpackRevert(ret); uerr == nil {
		ce.Reason = reason
		return ce
	}
	ce.Reason = hexutil.Encode(ret)
	return ce
}

// hexField reads a hex field of the contract document.
func hexField(fields map[string]any, key string) ([]byte, error) {
	switch v := fields[key].(type) {
	case nil:
		return nil, nil
	case []byte:
		return v, nil
	case string:
		b, err := decodeHex(v)
		if err != nil {
			return nil, &ContractError{Err: fmt.Errorf("invalid %s: %w", key, err)}
		}
		return b, nil
	}
	return nil, &ContractError{Err: fmt.Errorf("invalid %s: %T", key, fields[key])}
}

// membershipOf reads the membership functions of the contract document.
func membershipOf(v any) ([]string, error) {
	switch m := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return m, nil
	case []any:
		out := make([]string, len(m))
		for i, x := range m {
			name, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("contract document: invalid %s %v", KeyMembership, v)
			}
			out[i] = name
		}
		return out, nil
	}
	return nil, fmt.Errorf("contract document: invalid %s %v", KeyMembership, v)
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
	// The EVM has the precompiled contracts of the latest fork the chain
	// config activates (Osaka): ecrecover, sha256, ripemd160, identity,
	// modexp, bn254 add/mul/pairing, blake2f, KZG point evaluation, the
	// BLS12-381 operations and P-256 verification. Callers mark their
	// addresses warm (Prepare), as Ethereum does.
	evm := vm.NewEVM(blockCtx, statedb, chainConfig, vm.Config{})
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
