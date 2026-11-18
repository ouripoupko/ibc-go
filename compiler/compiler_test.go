package compiler

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/ouripoupko/ibc-go/identity"
)

// testCompiler is the compiler in IBC_SOLC (space-separated command), or
// solc on the PATH; the tests are skipped without one. A solc-js build runs
// as IBC_SOLC="node /abs/compiler/runner.cjs /abs/soljson-v….js".
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

func counter(t *testing.T) identity.Sources {
	t.Helper()
	src, err := os.ReadFile("../examples/contracts/Counter.sol")
	if err != nil {
		t.Fatal(err)
	}
	return identity.Sources{"Counter.sol": string(src)}
}

func TestCompile(t *testing.T) {
	s := testCompiler(t)
	src := counter(t)
	r, err := s.Compile(context.Background(), src, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Contract != "Counter.sol:Counter" || len(r.Code) < 100 || !strings.Contains(string(r.ABI), "increment") || r.Compiler.Version == "" {
		t.Fatalf("compile = %+v", r)
	}
	if r.SourceHash != identity.SourceHash(src).Hex() {
		t.Fatalf("source hash %s, want %s", r.SourceHash, identity.SourceHash(src).Hex())
	}
	again, _ := s.Compile(context.Background(), src, "Counter")
	if again.Code != r.Code {
		t.Fatal("the same source compiled to different code")
	}
	if _, err := s.Compile(context.Background(), identity.Sources{"Broken.sol": "contract Broken {"}, ""); err == nil || !strings.Contains(err.Error(), "does not compile") {
		t.Fatalf("broken source: %v", err)
	}
	if _, err := (&Solc{Command: []string{"no-such-compiler-here"}}).Compile(context.Background(), src, ""); err != ErrNoCompiler {
		t.Fatalf("missing compiler: %v", err)
	}
}

// A contract that imports a library from another file, by a relative path.
var withImport = identity.Sources{
	"lib/Math.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;
library Math {
    function max(uint256 a, uint256 b) internal pure returns (uint256) { return a > b ? a : b; }
}
`,
	"Calc.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;
import "./lib/Math.sol";
contract Calc {
    uint256 public top;
    function offer(uint256 n) external { top = Math.max(top, n); }
}
`,
}

func TestCompileImport(t *testing.T) {
	s := testCompiler(t)
	// The library compiles to code too: the deploy names its contract.
	if _, err := s.Compile(context.Background(), withImport, ""); err == nil || !strings.Contains(err.Error(), "say which") {
		t.Fatalf("no name: %v", err)
	}
	r, err := s.Compile(context.Background(), withImport, "Calc")
	if err != nil {
		t.Fatal(err)
	}
	if r.Contract != "Calc.sol:Calc" || len(r.ABIs) != 2 || len(r.Membership) != 0 || !strings.Contains(string(r.ABI), "offer") || r.SourceHash != identity.SourceHash(withImport).Hex() {
		t.Fatalf("compile = %+v", r)
	}
}

// Inheritance across files; and two files with contracts of the same name.
var withInheritance = identity.Sources{
	"base/Owned.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;
contract Owned {
    address public owner = msg.sender;
    modifier onlyOwner() { require(msg.sender == owner, "not the owner"); _; }
}
`,
	"Vault.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;
import {Owned} from "./base/Owned.sol";
import "./other/Vault.sol" as other;
contract Vault is Owned {
    uint256 public total;
    function add(uint256 n) external onlyOwner { total += n; }
}
`,
	"other/Vault.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;
contract Vault { function hello() external pure returns (uint256) { return 1; } }
`,
}

func TestCompileInheritance(t *testing.T) {
	s := testCompiler(t)
	ctx := context.Background()
	// Owned and both Vaults are deployable: the deploy must say which.
	if _, err := s.Compile(ctx, withInheritance, ""); err == nil || !strings.Contains(err.Error(), "say which") {
		t.Fatalf("no name: %v", err)
	}
	if _, err := s.Compile(ctx, withInheritance, "Vault"); err == nil || !strings.Contains(err.Error(), "path:Name") {
		t.Fatalf("an ambiguous name: %v", err)
	}
	r, err := s.Compile(ctx, withInheritance, "Vault.sol:Vault")
	if err != nil {
		t.Fatal(err)
	}
	var abi []struct{ Name string }
	if err := json.Unmarshal(r.ABI, &abi); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range abi {
		names[f.Name] = true
	}
	if r.Contract != "Vault.sol:Vault" || !names["add"] || !names["owner"] || names["hello"] {
		t.Fatalf("compile = %s, abi %v", r.Contract, names)
	}
	if other, err := s.Compile(ctx, withInheritance, "other/Vault.sol:Vault"); err != nil || other.Code == r.Code {
		t.Fatalf("the other Vault: %v", err)
	}
	if owned, err := s.Compile(ctx, withInheritance, "Owned"); err != nil || owned.Contract != "base/Owned.sol:Owned" {
		t.Fatalf("Owned: %v", err)
	}
}

// Imports resolve only within the source: nothing is downloaded or read.
func TestCompileMissingImport(t *testing.T) {
	s := testCompiler(t)
	for name, src := range map[string]identity.Sources{
		"library": {"Token.sol": `pragma solidity ^0.8.0;
import "@openzeppelin/contracts/token/ERC20/ERC20.sol";
contract Token is ERC20 { constructor() ERC20("T", "T") {} }
`},
		"file":    {"A.sol": "pragma solidity ^0.8.0;\nimport \"./B.sol\";\ncontract A {}\n"},
		"outside": {"A.sol": "pragma solidity ^0.8.0;\nimport \"../../go.mod\";\ncontract A {}\n"},
	} {
		if _, err := s.Compile(context.Background(), src, ""); !IsSourceError(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A joining node checks a multi-file deploy as a single-file one.
func TestCheckFiles(t *testing.T) {
	s := testCompiler(t)
	ctx := context.Background()
	version, err := s.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ShortVersion(version) != PinnedVersion {
		t.Skipf("the test compiler is %s, not %s", version, PinnedVersion)
	}
	tc := &Toolchain{Native: s}
	r, err := tc.Compile(ctx, withImport, "Calc")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(r.Compiler)
	if got, why := tc.Check(ctx, withImport, info, claimOf(r)); got != Verified {
		t.Fatalf("check = %s (%s)", got, why)
	}
	changed := identity.Sources{}
	for p, c := range withImport {
		changed[p] = c
	}
	changed["lib/Math.sol"] = strings.Replace(changed["lib/Math.sol"], "a > b", "a >= b", 1)
	if got, _ := tc.Check(ctx, changed, info, claimOf(r)); got != Mismatch {
		t.Fatalf("check of a changed file = %s", got)
	}
	// The paths are part of what is compiled.
	moved := identity.Sources{"Calc.sol": strings.Replace(withImport["Calc.sol"], "./lib/", "./util/", 1), "util/Math.sol": withImport["lib/Math.sol"]}
	if got, _ := tc.Check(ctx, moved, info, claimOf(r)); got != Mismatch {
		t.Fatalf("check of moved files = %s", got)
	}
}

// claimOf is what a deploy of r claims.
func claimOf(r Result) Claim {
	return Claim{Contract: r.Contract, Code: r.Code, ABIs: r.ABIs, Membership: r.Membership}
}

// A deploy carries the ABIs and the membership functions: the check compares
// them too.
func TestCheckClaim(t *testing.T) {
	s := testCompiler(t)
	ctx := context.Background()
	version, err := s.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ShortVersion(version) != PinnedVersion {
		t.Skipf("the test compiler is %s, not %s", version, PinnedVersion)
	}
	club := identity.Sources{"Club.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;
contract Club {
    mapping(address => bool) public invited;
    constructor() { invited[msg.sender] = true; }
    function approve_partner(address who) external view returns (bool) { return invited[who]; }
    function approve_removal(address) external pure returns (bool) { return true; }
}
`}
	tc := &Toolchain{Native: s}
	r, err := tc.Compile(ctx, club, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Membership, []string{"approve_partner", "approve_removal"}) {
		t.Fatalf("membership = %v", r.Membership)
	}
	info, _ := json.Marshal(r.Compiler)
	if got, why := tc.Check(ctx, club, info, claimOf(r)); got != Verified {
		t.Fatalf("check = %s (%s)", got, why)
	}
	lies := map[string]func(*Claim){
		"another contract": func(c *Claim) { c.Contract = "Club.sol:Other" },
		"fewer membership": func(c *Claim) { c.Membership = []string{"approve_partner"} },
		"more membership":  func(c *Claim) { c.Membership = append(c.Membership, "approve_guest") },
		"another ABI":      func(c *Claim) { c.ABIs = map[string]json.RawMessage{"Club.sol:Club": json.RawMessage("[]")} },
		"an ABI too many": func(c *Claim) {
			c.ABIs = map[string]json.RawMessage{"Club.sol:Club": r.ABI, "X.sol:X": json.RawMessage("[]")}
		},
	}
	for name, lie := range lies {
		c := claimOf(r)
		lie(&c)
		if got, _ := tc.Check(ctx, club, info, c); got != Mismatch {
			t.Errorf("%s: %s", name, got)
		}
	}
	// A membership function must return bool.
	bad := identity.Sources{"Bad.sol": "pragma solidity ^0.8.0;\ncontract Bad { function approve_guest(address) external pure returns (uint256) { return 1; } }\n"}
	if _, err := tc.Compile(ctx, bad, ""); !IsSourceError(err) || !strings.Contains(err.Error(), "must return bool") {
		t.Errorf("approve_guest returning uint256: %v", err)
	}
	// In a source, a signature means one function.
	clash := identity.Sources{"A.sol": "pragma solidity ^0.8.0;\ncontract A { function get() external pure returns (uint256) { return 1; } }\ncontract B { function get() external pure returns (string memory) { return \"b\"; } }\n"}
	if _, err := tc.Compile(ctx, clash, "A"); !IsSourceError(err) || !strings.Contains(err.Error(), "get()") {
		t.Errorf("get() returning uint256 and string: %v", err)
	}
}
