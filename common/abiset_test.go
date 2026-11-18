package common

import (
	"encoding/json"
	"strings"
	"testing"
)

func abis(t *testing.T, contracts map[string]string) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for name, a := range contracts {
		out[name] = json.RawMessage(a)
	}
	return out
}

const (
	fnGet    = `{"type":"function","name":"get","inputs":[],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"}`
	fnGetStr = `{"type":"function","name":"get","inputs":[],"outputs":[{"name":"","type":"string"}],"stateMutability":"view"}`
	fnSet    = `{"type":"function","name":"set","inputs":[{"name":"v","type":"uint256"}],"outputs":[],"stateMutability":"nonpayable"}`
	fnSetB   = `{"type":"function","name":"set","inputs":[{"name":"v","type":"bytes32"}],"outputs":[],"stateMutability":"nonpayable"}`
)

// A selector means one function in a source.
func TestABISetRule(t *testing.T) {
	// The same function in two contracts is one function of the source.
	set, err := NewABISet(abis(t, map[string]string{"A.sol:A": "[" + fnGet + "," + fnSet + "]", "B.sol:B": "[" + fnGet + "]"}))
	if err != nil {
		t.Fatal(err)
	}
	f, err := set.Function("get")
	if err != nil || strings.Join(f.Contracts, ",") != "A.sol:A,B.sol:B" {
		t.Fatalf("get: %v %v", f, err)
	}
	// One signature with other outputs is refused.
	if _, err := NewABISet(abis(t, map[string]string{"A.sol:A": "[" + fnGet + "]", "B.sol:B": "[" + fnGetStr + "]"})); err == nil {
		t.Error("get() returning uint256 and string accepted")
	}
	// Two signatures (overloads) are two functions: by name, ambiguous.
	set, err = NewABISet(abis(t, map[string]string{"A.sol:A": "[" + fnSet + "]", "B.sol:B": "[" + fnSetB + "]"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Function("set"); err == nil || !strings.Contains(err.Error(), "by its signature") {
		t.Errorf("ambiguous name: %v", err)
	}
	if f, err := set.Function("set(bytes32)"); err != nil || f.Contracts[0] != "B.sol:B" {
		t.Errorf("set(bytes32): %v %v", f, err)
	}
	if _, err := set.Function("nope"); err == nil || !strings.Contains(err.Error(), `unknown method "nope"`) {
		t.Errorf("unknown: %v", err)
	}
}

// Encoding checks the arguments against the function's inputs.
func TestEncodeArguments(t *testing.T) {
	set, err := NewABISet(abis(t, map[string]string{"A.sol:A": "[" + fnSet + "]"}))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := set.Function("set")
	for name, tc := range map[string]struct {
		values map[string]any
		want   string
	}{
		"missing argument":    {map[string]any{}, `missing argument "v"`},
		"unexpected argument": {map[string]any{"v": 1.0, "extra": 2.0}, `unexpected argument "extra"`},
		"negative uint":       {map[string]any{"v": -1.0}, "out of range"},
		"inexact float":       {map[string]any{"v": 1.5}, "not an exact integer"},
	} {
		if _, err := f.Encode(tc.values); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	data, err := f.Encode(map[string]any{"v": 5})
	if err != nil || len(data) != 36 || set.BySelector(data) != f {
		t.Fatalf("set(5) = %x, %v", data, err)
	}
	// One item, as the node's encode action takes it.
	if item, err := EncodeItem(json.RawMessage(fnSet), map[string]any{"v": 5}); err != nil || string(item) != string(data) {
		t.Errorf("EncodeItem = %x, %v", item, err)
	}
	ctor := `{"type":"constructor","inputs":[{"name":"n","type":"uint8"}],"stateMutability":"nonpayable"}`
	if args, err := EncodeItem(json.RawMessage(ctor), map[string]any{"n": 2}); err != nil || len(args) != 32 || args[31] != 2 {
		t.Errorf("constructor args = %x, %v", args, err)
	}
}
