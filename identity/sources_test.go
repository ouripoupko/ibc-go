package identity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestSourceHash(t *testing.T) {
	two := Sources{"A.sol": "import './lib/B.sol'; contract A {}", "lib/B.sol": "library B {}"}
	// The same files, decoded from JSON in another order, hash the same.
	var reordered Sources
	if err := json.Unmarshal([]byte(`{"lib/B.sol":"library B {}","A.sol":"import './lib/B.sol'; contract A {}"}`), &reordered); err != nil {
		t.Fatal(err)
	}
	if SourceHash(two) != SourceHash(reordered) {
		t.Fatal("the same files in another order hash differently")
	}
	// The rule, spelled out: files sorted by path, keccak(path) ‖ keccak(content).
	want := crypto.Keccak256Hash(
		crypto.Keccak256([]byte("A.sol")), crypto.Keccak256([]byte(two["A.sol"])),
		crypto.Keccak256([]byte("lib/B.sol")), crypto.Keccak256([]byte(two["lib/B.sol"])))
	if SourceHash(two) != want {
		t.Fatalf("hash %s, want %s", SourceHash(two).Hex(), want.Hex())
	}
	// A path, a content, or a file more or less changes it.
	for name, other := range map[string]Sources{
		"renamed":  {"A.sol": two["A.sol"], "lib/C.sol": two["lib/B.sol"]},
		"changed":  {"A.sol": two["A.sol"], "lib/B.sol": "library B { }"},
		"fewer":    {"A.sol": two["A.sol"]},
		"more":     {"A.sol": two["A.sol"], "lib/B.sol": two["lib/B.sol"], "C.sol": "contract C {}"},
		"swapped":  {"A.sol": two["lib/B.sol"], "lib/B.sol": two["A.sol"]},
		"one file": {"A.sol": two["A.sol"] + two["lib/B.sol"]},
	} {
		if SourceHash(other) == SourceHash(two) {
			t.Errorf("%s: same hash", name)
		}
	}
	// A single file is hashed with its path, by the same rule.
	one := Sources{"Counter.sol": "contract Counter {}"}
	if SourceHash(one) != crypto.Keccak256Hash(crypto.Keccak256([]byte("Counter.sol")), crypto.Keccak256([]byte("contract Counter {}"))) ||
		SourceHash(one) == SourceHash(Sources{"Other.sol": "contract Counter {}"}) {
		t.Fatal("a single file's hash")
	}
}

func TestSourcesValidate(t *testing.T) {
	if err := (Sources{"A.sol": "x", "lib/b-c_d.v2.sol": "y"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Sources{
		"none":       {},
		"absolute":   {"/A.sol": "x"},
		"parent":     {"../A.sol": "x"},
		"dot":        {"./A.sol": "x"},
		"inner dot":  {"lib/../A.sol": "x"},
		"empty part": {"lib//A.sol": "x"},
		"not .sol":   {"A.txt": "x"},
		"backslash":  {`lib\A.sol`: "x"},
		"space":      {"my file.sol": "x"},
		"unicode":    {"é.sol": "x"},
		"remapping":  {"@oz/A.sol": "x"},
		"empty file": {"A.sol": ""},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	many := Sources{}
	for i := 0; i <= MaxSourceFiles; i++ {
		many[strings.Repeat("a", i+1)+".sol"] = "x"
	}
	if err := many.Validate(); err == nil {
		t.Error("too many files: valid")
	}
	// A deploy body's source must be a valid map of files.
	for _, src := range []string{`"contract A {}"`, `{}`, `{"/A.sol":"x"}`} {
		tx := Transaction{Kind: KindDeploy, Body: `{"code":"6001","abi":[],"nodeKey":"0x0000000000000000000000000000000000000001","compiler":{"version":"x"},"source":` + src + `}`}
		if _, err := tx.ParseBody(); err == nil {
			t.Errorf("deploy with source %s parsed", src)
		}
	}
}
