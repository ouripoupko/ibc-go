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
)

// fakeSoljson is a stand-in for a solc-js build: it compiles every source to
// the code 0x6001, unless the source says "broken".
const fakeSoljson = `module.exports = { cwrap: (name) => name === 'solidity_version'
  ? () => '0.8.38+commit.abcdef12.Emscripten.clang'
  : (input) => JSON.parse(input).sources['contract.sol'].content.includes('broken')
    ? JSON.stringify({ errors: [{ severity: 'error', formattedMessage: 'ParserError: broken' }] })
    : JSON.stringify({ contracts: { 'contract.sol': { A: { abi: [], evm: { bytecode: { object: '6001' } } } } } }) };
`

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
	r, err := tc.Compile(ctx, "contract A {}", "")
	if err != nil || r.Code != "6001" || r.Compiler.Version != "0.8.38+commit.abcdef12.Emscripten.clang" {
		t.Fatalf("compile = %+v, %v", r, err)
	}
	info, _ := json.Marshal(r.Compiler)
	if got, why := tc.Check(ctx, "contract A {}", info, "0x6001"); got != Verified {
		t.Errorf("check of the same code = %s (%s)", got, why)
	}
	if got, _ := tc.Check(ctx, "contract A {}", info, "0x6002"); got != Mismatch {
		t.Errorf("check of other code = %s", got)
	}
	if got, _ := tc.Check(ctx, "broken", info, "0x6001"); got != Mismatch {
		t.Errorf("check of a source that does not compile = %s", got)
	}
	other, _ := json.Marshal(Info{Version: "0.8.37+commit.11111111.Linux.g++"})
	if got, why := tc.Check(ctx, "contract A {}", other, "0x6001"); got != Unchecked || !strings.Contains(why, "0.8.37") {
		t.Errorf("check with a version not to be found = %s (%s)", got, why)
	}
	if got, _ := tc.Check(ctx, "contract A {}", []byte(`{}`), "0x6001"); got != Mismatch {
		t.Errorf("check without a compiler = %s", got)
	}
}

func TestToolchainRefusesABadDownload(t *testing.T) {
	srv := releases(t, strings.Repeat("00", 32))
	tc := &Toolchain{Dir: t.TempDir(), BaseURL: srv.URL}
	if _, err := tc.Compile(context.Background(), "contract A {}", ""); err == nil || !strings.Contains(err.Error(), "hash") {
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
	r, err := tc.Compile(context.Background(), string(src), "")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(r.Compiler)
	if got, why := tc.Check(context.Background(), string(src), info, r.Code); got != Verified {
		t.Fatalf("check = %s (%s)", got, why)
	}
	t.Logf("compiled with %s", r.Compiler.Version)
}
