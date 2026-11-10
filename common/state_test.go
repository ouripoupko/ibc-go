package common

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
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

func registryDoc(t *testing.T) *MemoryDocument {
	t.Helper()
	bin, err := os.ReadFile("../testdata/Registry.bin")
	if err != nil {
		t.Fatal(err)
	}
	abiJSON, err := os.ReadFile("../testdata/Registry.abi")
	if err != nil {
		t.Fatal(err)
	}
	return NewMemoryDocument(map[string]any{
		KeyPID:         creator,
		KeyAddress:     "http://alice.example",
		KeyCode:        strings.TrimSpace(string(bin)),
		KeyABI:         string(abiJSON),
		KeyConstructor: map[string]any{"initialName": "registry", "start": json.Number("7")},
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
	if _, err := s.Call("bob", "setValue", map[string]any{"value": 5.0}, t1); err != nil {
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
	res, err := reloaded.View("bob", "getValue", map[string]any{"who": AgentAddress("bob").Hex()}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Value != json.Number("5") {
		t.Fatalf("getValue after restart = %v, want 5", res.Value)
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

func TestArgumentErrors(t *testing.T) {
	s, _, _ := newRunningState(t)
	root := s.Root()
	cases := map[string]struct {
		method string
		values map[string]any
		want   string
	}{
		"unknown method":      {"nope", nil, `unknown method "nope"`},
		"missing argument":    {"setValue", map[string]any{}, `missing argument "value"`},
		"unexpected argument": {"setValue", map[string]any{"value": 1.0, "extra": 2.0}, `unexpected argument "extra"`},
		"negative uint":       {"setValue", map[string]any{"value": -1.0}, "out of range"},
		"inexact float":       {"setValue", map[string]any{"value": 1.5}, "not an exact integer"},
		"bad address":         {"getValue", map[string]any{"who": "bob"}, "invalid address"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Arguments are checked the same way in a transaction (Call) and a read (View).
			call := s.Call
			if tc.method == "getValue" {
				call = s.View
			}
			_, err := call("bob", tc.method, tc.values, t1)
			var ce *ContractError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ContractError containing %q", err, tc.want)
			}
		})
	}
	if s.Root() != root {
		t.Fatal("argument errors changed the root")
	}
}

func TestLargeIntegersAndUnnamedArguments(t *testing.T) {
	s, _, _ := newRunningState(t)
	big := "115792089237316195423570985008687907853269984665640564039457584007913129639935" // 2^256-1
	if _, err := s.Call("bob", "setValue", map[string]any{"value": big}, t1); err != nil {
		t.Fatal(err)
	}
	// The auto-generated getter values(address) has an unnamed input: key "0".
	res, err := s.View("bob", "values", map[string]any{"0": AgentAddress("bob").Hex()}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Value != json.Number(big) {
		t.Fatalf("values(bob) = %v, want %s", res.Value, big)
	}
	out, _ := json.Marshal(res.Value)
	if string(out) != big {
		t.Fatalf("JSON = %s, want %s", out, big)
	}
}

func TestRevertIsContractError(t *testing.T) {
	s, _, _ := newRunningState(t)
	_, err := s.Call("bob", "setThenFail", map[string]any{"value": 1.0}, t1)
	var ce *ContractError
	if !errors.As(err, &ce) || !errors.Is(err, vm.ErrExecutionReverted) {
		t.Fatalf("err = %v, want ContractError wrapping ErrExecutionReverted", err)
	}
	if ce.Reason != "setThenFail: rejected" {
		t.Fatalf("reason = %q", ce.Reason)
	}
	if err.Error() != "execution reverted: setThenFail: rejected" {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestNotRunning(t *testing.T) {
	s := NewState(registryDoc(t), NewMemoryCollection(), rawdb.NewMemoryDatabase())
	if _, err := s.Call("bob", "setValue", map[string]any{"value": 1.0}, t1); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}
