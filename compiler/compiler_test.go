package compiler

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// testCompiler is the compiler in IBC_SOLC (space-separated command), or
// solc on the PATH; the tests are skipped without one.
func testCompiler(t *testing.T) *Solc {
	t.Helper()
	if cmd := os.Getenv("IBC_SOLC"); cmd != "" {
		return &Solc{Command: strings.Fields(cmd)}
	}
	if _, err := exec.LookPath("solc"); err == nil {
		return &Solc{Command: []string{"solc"}}
	}
	t.Skip("no Solidity compiler: set IBC_SOLC or put solc on the PATH")
	return nil
}

func TestCompile(t *testing.T) {
	s := testCompiler(t)
	src, err := os.ReadFile("../examples/contracts/Counter.sol")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Compile(context.Background(), string(src), "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Contract != "Counter" || len(r.Code) < 100 || !strings.Contains(string(r.ABI), "increment") || r.Compiler.Version == "" {
		t.Fatalf("compile = %+v", r)
	}
	again, _ := s.Compile(context.Background(), string(src), "Counter")
	if again.CodeHash != r.CodeHash {
		t.Fatal("the same source compiled to different code")
	}
	if _, err := s.Compile(context.Background(), "contract Broken {", ""); err == nil || !strings.Contains(err.Error(), "does not compile") {
		t.Fatalf("broken source: %v", err)
	}
	if _, err := (&Solc{Command: []string{"no-such-compiler-here"}}).Compile(context.Background(), string(src), ""); err != ErrNoCompiler {
		t.Fatalf("missing compiler: %v", err)
	}
}
