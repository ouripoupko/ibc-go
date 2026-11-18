package common

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

const (
	creator = "alice-pid"
	t0      = "20240102030405123456"
	t1      = "20240102030500000000"
)

// testContract is a compiled test contract: its creation code and the ABIs
// of its source, from testdata.
type testContract struct {
	code string
	abis *ABISet
}

func readContract(t *testing.T, name string) testContract {
	t.Helper()
	bin, err := os.ReadFile("../testdata/" + name + ".bin")
	if err != nil {
		t.Fatal(err)
	}
	abis := map[string]json.RawMessage{}
	if raw, err := os.ReadFile("../testdata/" + name + ".abis.json"); err == nil {
		if err := json.Unmarshal(raw, &abis); err != nil {
			t.Fatal(err)
		}
	} else {
		raw, err := os.ReadFile("../testdata/" + name + ".abi")
		if err != nil {
			t.Fatal(err)
		}
		abis[name+".sol:"+name] = raw
	}
	set, err := NewABISet(abis)
	if err != nil {
		t.Fatal(err)
	}
	return testContract{code: strings.TrimSpace(string(bin)), abis: set}
}

// encode is the calldata of a call to the source's function method.
func (c testContract) encode(t *testing.T, method string, values map[string]any) []byte {
	t.Helper()
	f, err := c.abis.Function(method)
	if err != nil {
		t.Fatal(err)
	}
	if values == nil {
		values = map[string]any{}
	}
	data, err := f.Encode(values)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// decode decodes a call's hex result with the function's outputs.
func (c testContract) decode(t *testing.T, method string, value any) any {
	t.Helper()
	f, err := c.abis.Function(method)
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.Decode(hexutil.MustDecode(value.(string)))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// args encodes constructor arguments for the root contract name.
func (c testContract) args(t *testing.T, name string, values map[string]any) string {
	t.Helper()
	a, _ := c.abis.Contract(name)
	packed, err := packArguments(a.Constructor.Inputs, values)
	if err != nil {
		t.Fatal(err)
	}
	return hexutil.Encode(packed)
}

func registry(t *testing.T) testContract { return readContract(t, "Registry") }

func registryDoc(t *testing.T) *MemoryDocument {
	t.Helper()
	r := registry(t)
	return NewMemoryDocument(map[string]any{
		KeyPID:     creator,
		KeyAddress: "http://alice.example",
		KeyCode:    r.code,
		KeyArgs:    r.args(t, "Registry.sol:Registry", map[string]any{"initialName": "registry", "start": json.Number("7")}),
	})
}

func newRunningState(t *testing.T) (*State, *MemoryDocument, ethdb.Database) {
	t.Helper()
	doc := registryDoc(t)
	db := rawdb.NewMemoryDatabase()
	s := NewState(doc, NewMemoryCollection(), db)
	if err := s.Run(creator, t0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return s, doc, db
}

func TestAgentAddress(t *testing.T) {
	want := ethcommon.BytesToAddress(crypto.Keccak256([]byte("bob"))[12:])
	if got := AgentAddress("bob"); got != want {
		t.Fatalf("AgentAddress = %s, want %s", got, want)
	}
}

func TestParseTimestamp(t *testing.T) {
	got, err := ParseTimestamp("20240102030405123456")
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(1704164645); got != want { // 2024-01-02T03:04:05Z
		t.Fatalf("ParseTimestamp = %d, want %d", got, want)
	}
	for _, bad := range []string{"", "2024", "2024010203040x123456", "20241302030405000000", "202401020304051234567"} {
		if _, err := ParseTimestamp(bad); err == nil {
			t.Errorf("ParseTimestamp(%q) succeeded, want error", bad)
		}
	}
}

func TestDeployStoresAddressRootAndBlockNumber(t *testing.T) {
	s, doc, _ := newRunningState(t)
	fields := doc.Snapshot()
	if fields[KeyEVMAddress] != s.Address().Hex() {
		t.Errorf("evm_address = %v, want %s", fields[KeyEVMAddress], s.Address().Hex())
	}
	if want := crypto.CreateAddress(AgentAddress(creator), 0); s.Address() != want {
		t.Errorf("contract address = %s, want CREATE(creator, 0) = %s", s.Address(), want)
	}
	if fields[KeyStateRoot] != s.Root().Hex() {
		t.Errorf("state_root = %v, want %s", fields[KeyStateRoot], s.Root().Hex())
	}
	if fields[KeyBlockNumber] != uint64(1) {
		t.Errorf("block_number = %v, want 1", fields[KeyBlockNumber])
	}
	if fields[KeyAddress] != "http://alice.example" {
		t.Errorf("creator network address was overwritten: %v", fields[KeyAddress])
	}
}

func TestRunReopensStateWithoutRedeploying(t *testing.T) {
	s, doc, db := newRunningState(t)
	r := registry(t)
	if _, err := s.Call("bob", nil, r.encode(t, "setValue", map[string]any{"value": 5.0}), t1); err != nil {
		t.Fatal(err)
	}
	root := s.Root()

	// A new State over the same document and store, as after a restart.
	reloaded := NewState(doc, NewMemoryCollection(), db)
	if err := reloaded.Run(creator, t0); err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if reloaded.Root() != root || reloaded.BlockNumber() != 2 {
		t.Fatalf("reloaded root %s block %d, want %s block 2", reloaded.Root(), reloaded.BlockNumber(), root)
	}
	res, err := reloaded.View("bob", nil, r.encode(t, "getValue", map[string]any{"who": AgentAddress("bob").Hex()}), "")
	if err != nil {
		t.Fatal(err)
	}
	if v := r.decode(t, "getValue", res.Value); v != json.Number("5") {
		t.Fatalf("getValue after restart = %v, want 5", v)
	}
}

func TestRunFailsOnMissingState(t *testing.T) {
	_, doc, _ := newRunningState(t)
	// Same document, but an empty store: the stored root cannot be opened.
	s := NewState(doc, NewMemoryCollection(), rawdb.NewMemoryDatabase())
	if err := s.Run(creator, t0); err == nil {
		t.Fatal("Run succeeded on a store without the contract's state")
	}
}

// Execution runs calldata: no ABI. A call returns its return data, as hex.
func TestCallReturnsData(t *testing.T) {
	s, _, _ := newRunningState(t)
	r := registry(t)
	res, err := s.Call("bob", nil, r.encode(t, "setValue", map[string]any{"value": 9.0}), t1)
	if err != nil {
		t.Fatal(err)
	}
	if ret, ok := res.Value.(string); !ok || len(ret) != 2+64 {
		t.Fatalf("setValue = %+v: want one word of return data, hex", res)
	}
	big := "115792089237316195423570985008687907853269984665640564039457584007913129639935" // 2^256-1
	if _, err := s.Call("bob", nil, r.encode(t, "setValue", map[string]any{"value": big}), t1); err != nil {
		t.Fatal(err)
	}
	// The auto-generated getter values(address) has an unnamed input: key "0".
	res, err = s.View("bob", nil, r.encode(t, "values", map[string]any{"0": AgentAddress("bob").Hex()}), "")
	if err != nil {
		t.Fatal(err)
	}
	if v := r.decode(t, "values", res.Value); v != json.Number(big) {
		t.Fatalf("values(bob) = %v, want %s", v, big)
	}
	// Data no function answers reverts, and changes nothing.
	root := s.Root()
	if _, err := s.Call("bob", nil, []byte{1, 2, 3, 4}, t1); !errors.Is(err, vm.ErrExecutionReverted) || s.Root() != root {
		t.Fatalf("unknown selector: %v", err)
	}
}

func TestRevertIsContractError(t *testing.T) {
	s, _, _ := newRunningState(t)
	_, err := s.Call("bob", nil, registry(t).encode(t, "setThenFail", map[string]any{"value": 1.0}), t1)
	var ce *ContractError
	if !errors.As(err, &ce) || !errors.Is(err, vm.ErrExecutionReverted) {
		t.Fatalf("err = %v, want ContractError wrapping ErrExecutionReverted", err)
	}
	if ce.Reason != "setThenFail: rejected" || len(ce.Data) == 0 {
		t.Fatalf("reason = %q, data %x", ce.Reason, ce.Data)
	}
	if err.Error() != "execution reverted: setThenFail: rejected" {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestNotRunning(t *testing.T) {
	s := NewState(registryDoc(t), NewMemoryCollection(), rawdb.NewMemoryDatabase())
	if _, err := s.Call("bob", nil, []byte{1, 2, 3, 4}, t1); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

// worldState runs testdata/World.sol: a root contract that creates a Tally,
// and defines approve_partner.
func worldState(t *testing.T) (*State, testContract) {
	t.Helper()
	w := readContract(t, "World")
	doc := NewMemoryDocument(map[string]any{KeyPID: creator, KeyCode: w.code,
		KeyMembership: []any{"approve_partner"}})
	s := NewState(doc, NewMemoryCollection(), rawdb.NewMemoryDatabase())
	if err := s.Run(creator, t0); err != nil {
		t.Fatal(err)
	}
	return s, w
}

// A call reaches any contract in the agreement's world, with msg.sender the
// agent.
func TestCallAnotherContract(t *testing.T) {
	s, w := worldState(t)
	res, err := s.View("bob", nil, w.encode(t, "tally", nil), "")
	if err != nil {
		t.Fatal(err)
	}
	tally := ethcommon.HexToAddress(w.decode(t, "tally", res.Value).(string))
	if res, err = s.Call("bob", &tally, w.encode(t, "add", map[string]any{"n": 3}), t1); err != nil {
		t.Fatal(err)
	}
	if v := w.decode(t, "add", res.Value); v != json.Number("3") {
		t.Fatalf("add(3) = %v", v)
	}
	res, _ = s.View("carol", &tally, w.encode(t, "last", nil), "")
	if v := w.decode(t, "last", res.Value); v != AgentAddress("bob").Hex() {
		t.Fatalf("last = %v, want bob", v)
	}
	// A custom error: execution keeps its data; the source's ABIs decode it.
	_, err = s.Call("bob", &tally, w.encode(t, "add", map[string]any{"n": 101}), t1)
	var ce *ContractError
	if !errors.As(err, &ce) || w.abis.DecodeRevert(ce.Data) != "TooMuch(101)" {
		t.Fatalf("add(101): %v", err)
	}
}

// The root contract decides the membership changes whose function it defines
// (the deploy lists them); the others get the default.
func TestDecide(t *testing.T) {
	s, w := worldState(t)
	bob, carol := AgentAddress("bob"), AgentAddress("carol")
	if ok, _, err := s.Decide("bob", "approve_partner", bob, t1, "j1", true); err != nil || ok {
		t.Fatalf("bob, not invited: %v %v", ok, err)
	}
	if _, err := s.Call(creator, nil, w.encode(t, "invite", map[string]any{"who": bob.Hex()}), t1); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := s.Decide("bob", "approve_partner", bob, t1, "j2", false); err != nil || !ok {
		t.Fatalf("bob, invited: %v %v", ok, err)
	}
	// approve_guest is not the contract's: the default decides, uncalled.
	for _, def := range []bool{true, false} {
		if ok, _, err := s.Decide("carol", "approve_guest", carol, t1, "g", def); err != nil || ok != def {
			t.Fatalf("approve_guest, default %v: %v %v", def, ok, err)
		}
	}
}
