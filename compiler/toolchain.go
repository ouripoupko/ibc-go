package compiler

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// runnerJS runs a solc-js build (a soljson file) as a compiler: with
// --version, or with standard JSON input on stdin.
//
//go:embed runner.cjs
var runnerJS []byte

// The results of Check.
const (
	Verified  = "verified"  // the source compiles to the code
	Unchecked = "unchecked" // the compiler could not be run (the reason says why)
	Mismatch  = "mismatch"  // the source does not compile to the code
)

// Toolchain is the node's compilers. It compiles deploys with PinnedVersion:
// with Native if that is its version, or else with the solc-js build of that
// version. To check a deploy, it compiles with the deploy's own version, the
// same way. It downloads solc-js builds from Solidity's release site the first
// time it needs them, checks them against the release list's hash, and keeps
// them in Dir; Node.js (Node) runs them.
type Toolchain struct {
	Native  *Solc        // the solc or solcjs command (-solc); may be nil
	Dir     string       // where downloaded compilers are kept
	Node    string       // the Node.js program; default "node"
	BaseURL string       // default https://binaries.soliditylang.org/bin
	HTTP    *http.Client // default: a client with a two-minute timeout
	Timeout time.Duration

	mu     sync.Mutex // one download, and one compilation, at a time
	native string     // Native's version, once known ("-" if it cannot run)
}

// soljson is a downloaded solc-js build, run by Node.js.
func (t *Toolchain) soljson(path string) runner {
	node := t.Node
	if node == "" {
		node = "node"
	}
	return func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		runner := filepath.Join(t.Dir, "runner.cjs")
		if err := os.WriteFile(runner, runnerJS, 0o644); err != nil {
			return nil, err
		}
		out, err := runCommand(ctx, t.Timeout, []string{node, runner, path}, stdin, args...)
		if errors.Is(err, ErrNoCompiler) {
			return nil, fmt.Errorf("running solc-js needs Node.js (%s): it is not installed", node)
		}
		return out, err
	}
}

// nativeVersion returns Native's version, or "" if there is none.
func (t *Toolchain) nativeVersion(ctx context.Context) string {
	if t.native == "" {
		t.native = "-"
		if v, err := t.Native.Version(ctx); err == nil {
			t.native = v
		}
	}
	if t.native == "-" {
		return ""
	}
	return t.native
}

// compilerFor returns a runner for a compiler version, a release ("0.8.38")
// or a long version ("0.8.38+commit.1835d247"), and that compiler's own
// version string. t.mu is held.
func (t *Toolchain) compilerFor(ctx context.Context, version string) (runner, string, error) {
	if v := t.nativeVersion(ctx); v != "" && (ShortVersion(v) == version || LongVersion(v) == version) {
		return t.Native.run, v, nil
	}
	if t.Dir == "" {
		return nil, "", fmt.Errorf("no solc %s on this node, and nowhere to download it", version)
	}
	path, err := t.download(ctx, version)
	if err != nil {
		return nil, "", fmt.Errorf("solc %s: %w", version, err)
	}
	run := t.soljson(path)
	out, err := run(ctx, nil, "--version")
	if err != nil {
		return nil, "", fmt.Errorf("solc %s: %w", version, err)
	}
	v, err := versionOf(out)
	if err != nil {
		return nil, "", err
	}
	return run, v, nil
}

// download returns the local path of the solc-js build of version,
// downloading it first if needed.
func (t *Toolchain) download(ctx context.Context, version string) (string, error) {
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return "", err
	}
	// A build already here was checked when it was downloaded.
	if matches, _ := filepath.Glob(filepath.Join(t.Dir, "soljson-v"+version+"*.js")); len(matches) > 0 {
		for _, m := range matches {
			if name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "soljson-v"), ".js"); name == version ||
				LongVersion(name) == version || (ShortVersion(name) == version && !strings.Contains(name, "-")) {
				return m, nil
			}
		}
	}
	base := t.BaseURL
	if base == "" {
		base = "https://binaries.soliditylang.org/bin"
	}
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	get := func(url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", url, res.Status)
		}
		return io.ReadAll(io.LimitReader(res.Body, 64<<20))
	}
	raw, err := get(base + "/list.json")
	if err != nil {
		return "", err
	}
	var list struct {
		Builds []struct {
			Path        string `json:"path"`
			Version     string `json:"version"`
			Prerelease  string `json:"prerelease"`
			LongVersion string `json:"longVersion"`
			SHA256      string `json:"sha256"`
		} `json:"builds"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", fmt.Errorf("release list: %w", err)
	}
	for _, b := range list.Builds {
		if b.LongVersion != version && (b.Version != version || b.Prerelease != "") {
			continue
		}
		if b.Path != filepath.Base(b.Path) || !strings.HasPrefix(b.Path, "soljson-") {
			return "", fmt.Errorf("unexpected build path %q", b.Path)
		}
		body, err := get(base + "/" + b.Path)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(body)
		if want := strings.TrimPrefix(b.SHA256, "0x"); !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
			return "", fmt.Errorf("%s: the download does not match the release list's hash", b.Path)
		}
		path := filepath.Join(t.Dir, b.Path)
		tmp := path + ".part"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			return "", err
		}
		return path, os.Rename(tmp, path)
	}
	return "", errors.New("no such release on soliditylang.org")
}

// Compile compiles source with the pinned version and the node's Settings.
func (t *Toolchain) Compile(ctx context.Context, source, name string) (Result, error) {
	if t == nil {
		return Result{}, ErrNoCompiler
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	run, version, err := t.compilerFor(ctx, PinnedVersion)
	if err != nil {
		return Result{}, err
	}
	return compileResult(ctx, run, version, source, name)
}

// Describe says which compiler the node compiles deploys with, for its log
// at startup. It downloads that compiler if needed.
func (t *Toolchain) Describe(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	native := t.nativeVersion(ctx)
	_, version, err := t.compilerFor(ctx, PinnedVersion)
	if err != nil {
		return "", err
	}
	if native == version {
		return "solc " + version, nil
	}
	if native != "" {
		return fmt.Sprintf("solc-js %s (the -solc compiler is %s, not %s)", version, native, PinnedVersion), nil
	}
	return "solc-js " + version, nil
}

// Check compiles a deploy's source with the deploy's compiler and settings,
// and compares the result with the deploy's code: Verified, Mismatch, or
// Unchecked with the reason it could not be compiled.
func (t *Toolchain) Check(ctx context.Context, source string, compiler json.RawMessage, code string) (string, string) {
	if t == nil {
		return Unchecked, "this node has no compiler"
	}
	var info Info
	if err := json.Unmarshal(compiler, &info); err != nil || info.Version == "" {
		return Mismatch, "the deploy does not say which compiler compiled it"
	}
	version := LongVersion(info.Version)
	if version == "" {
		return Mismatch, fmt.Sprintf("the deploy names an unknown compiler version %q", info.Version)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	run, _, err := t.compilerFor(ctx, version)
	if err != nil {
		return Unchecked, err.Error()
	}
	contracts, err := standardJSON(ctx, run, source, info.Settings)
	if IsSourceError(err) {
		return Mismatch, fmt.Sprintf("the source does not compile with solc %s", version)
	}
	if err != nil {
		return Unchecked, err.Error()
	}
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(code), "0x"))
	for _, c := range contracts {
		if strings.ToLower(strings.TrimPrefix(c.Code, "0x")) == want {
			return Verified, ""
		}
	}
	return Mismatch, fmt.Sprintf("the source, compiled with solc %s, is not the deployed code", version)
}
