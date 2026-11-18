package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ouripoupko/ibc-go/identity"
)

// fakeSoljson is a stand-in for a solc-js build: it compiles every source to
// one contract A, with the code 0x6001, in the first file; unless a file
// says "broken", or "reads", which makes it claim to have read a file that is
// not in the source.
const fakeSoljson = `module.exports = { cwrap: (name) => name === 'solidity_version'
  ? () => '0.8.38+commit.abcdef12.Emscripten.clang'
  : (input) => {
    const sources = JSON.parse(input).sources;
    const paths = Object.keys(sources).sort();
    const text = paths.map((p) => sources[p].content).join('\n');
    if (text.includes('broken')) return JSON.stringify({ errors: [{ severity: 'error', formattedMessage: 'ParserError: broken' }] });
    const read = Object.fromEntries(paths.map((p, id) => [p, { id }]));
    if (text.includes('reads')) read['elsewhere.sol'] = { id: paths.length };
    return JSON.stringify({ sources: read, contracts: { [paths[0]]: { A: { abi: [], evm: { bytecode: { object: '6001' } } } } } });
  } };
`

// one is a single-file source.
func one(text string) identity.Sources { return identity.Sources{"A.sol": text} }

// releases serves a release list with the fake build; hash is its sha256 in the list.
func releases(t *testing.T, hash string) *httptest.Server {
	t.Helper()
	path := "soljson-v0.8.38+commit.abcdef12.js"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list.json":
			_ = json.NewEncoder(w).Encode(map[string]any{"builds": []map[string]any{{"path": path, "version": "0.8.38",
				"longVersion": "0.8.38+commit.abcdef12", "sha256": "0x" + hash}}})
		case "/" + path:
			_, _ = w.Write([]byte(fakeSoljson))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestToolchain(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no Node.js")
	}
	sum := sha256.Sum256([]byte(fakeSoljson))
	srv := releases(t, hex.EncodeToString(sum[:]))
	tc := &Toolchain{Dir: t.TempDir(), BaseURL: srv.URL}
	ctx := context.Background()

	// Deploys compile with the pinned version, downloaded.
	r, err := tc.Compile(ctx, one("contract A {}"), "")
	if err != nil || r.Code != "6001" || r.Compiler.Version != "0.8.38+commit.abcdef12.Emscripten.clang" {
		t.Fatalf("compile = %+v, %v", r, err)
	}
	info, _ := json.Marshal(r.Compiler)
	claim := func(code string) Claim {
		return Claim{Contract: "A.sol:A", Code: code, ABIs: map[string]json.RawMessage{"A.sol:A": json.RawMessage("[]")}, Membership: []string{}}
	}
	if got, why := tc.Check(ctx, one("contract A {}"), info, claim("0x6001")); got != Verified {
		t.Errorf("check of the same code = %s (%s)", got, why)
	}
	if got, _ := tc.Check(ctx, one("contract A {}"), info, claim("0x6002")); got != Mismatch {
		t.Errorf("check of other code = %s", got)
	}
	if got, _ := tc.Check(ctx, one("broken"), info, claim("0x6001")); got != Mismatch {
		t.Errorf("check of a source that does not compile = %s", got)
	}
	other, _ := json.Marshal(Info{Version: "0.8.37+commit.11111111.Linux.g++"})
	if got, why := tc.Check(ctx, one("contract A {}"), other, claim("0x6001")); got != Unchecked || !strings.Contains(why, "0.8.37") {
		t.Errorf("check with a version not to be found = %s (%s)", got, why)
	}
	if got, _ := tc.Check(ctx, one("contract A {}"), []byte(`{}`), claim("0x6001")); got != Mismatch {
		t.Errorf("check without a compiler = %s", got)
	}

	// Several files go to the compiler together; one it read from elsewhere
	// fails the compilation.
	two := identity.Sources{"A.sol": "import './lib/B.sol'; contract A {}", "lib/B.sol": "library B {}"}
	if r, err := tc.Compile(ctx, two, ""); err != nil || r.Contract != "A.sol:A" || r.SourceHash != identity.SourceHash(two).Hex() {
		t.Errorf("compile of two files = %+v, %v", r, err)
	}
	if got, why := tc.Check(ctx, two, info, claim("0x6001")); got != Verified {
		t.Errorf("check of two files = %s (%s)", got, why)
	}
	if _, err := tc.Compile(ctx, one("reads"), ""); !IsSourceError(err) || !strings.Contains(err.Error(), "elsewhere.sol") {
		t.Errorf("compile that read another file: %v", err)
	}
	if _, err := tc.Compile(ctx, identity.Sources{"/abs.sol": "contract A {}"}, ""); err == nil {
		t.Error("compiled a source with an absolute path")
	}
}

func TestToolchainRefusesABadDownload(t *testing.T) {
	srv := releases(t, strings.Repeat("00", 32))
	tc := &Toolchain{Dir: t.TempDir(), BaseURL: srv.URL}
	if _, err := tc.Compile(context.Background(), one("contract A {}"), ""); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("compile with a bad download: %v", err)
	}
}

func TestVersions(t *testing.T) {
	for v, want := range map[string][2]string{
		"0.8.38+commit.1835d247.Linux.g++":            {"0.8.38+commit.1835d247", "0.8.38"},
		"0.8.38+commit.1835d247.Emscripten.clang":     {"0.8.38+commit.1835d247", "0.8.38"},
		"0.8.38-nightly.2026.9.10+commit.8310b69a.js": {"0.8.38-nightly.2026.9.10+commit.8310b69a", ""},
		"0.8.38": {"", ""},
	} {
		if LongVersion(v) != want[0] || ShortVersion(v) != want[1] {
			t.Errorf("%s: %q %q", v, LongVersion(v), ShortVersion(v))
		}
	}
}

// With IBC_SOLC_DOWNLOAD=1: downloads the pinned solc-js from soliditylang.org,
// and checks what it compiles.
func TestRealDownload(t *testing.T) {
	if os.Getenv("IBC_SOLC_DOWNLOAD") != "1" {
		t.Skip("set IBC_SOLC_DOWNLOAD=1 to download solc-js")
	}
	src, err := os.ReadFile("../examples/contracts/Counter.sol")
	if err != nil {
		t.Fatal(err)
	}
	tc := &Toolchain{Dir: t.TempDir()}
	r, err := tc.Compile(context.Background(), identity.Sources{"Counter.sol": string(src)}, "")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(r.Compiler)
	if got, why := tc.Check(context.Background(), identity.Sources{"Counter.sol": string(src)}, info, claimOf(r)); got != Verified {
		t.Fatalf("check = %s (%s)", got, why)
	}
	t.Logf("compiled with %s", r.Compiler.Version)
}
