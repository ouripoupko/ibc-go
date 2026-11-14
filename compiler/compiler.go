// Package compiler compiles Solidity source on the node, so that apps send
// source code and the node, not the app, produces the bytecode a user signs.
// It runs compilers that speak solc's standard JSON: the native solc, or
// solc-js (solcjs), given on the command line (Solc); or solc-js builds of any
// version, which the node downloads and runs with Node.js (Toolchain). The
// version and settings go into the deploy transaction, so that a joining node
// compiles the source again, with that version, and compares (Check).
package compiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ouripoupko/ibc-go/identity"
)

// PinnedVersion is the Solidity compiler this release of ibc compiles
// deploys with, on every node.
const PinnedVersion = "0.8.38"

// Settings are the compiler settings the node uses, fixed so that the same
// source always gives the same bytecode.
var Settings = map[string]any{
	"optimizer": map[string]any{"enabled": true, "runs": 200},
}

// Result is a compiled contract.
type Result struct {
	Contract   string          `json:"contract"` // the contract's name in the source
	Code       string          `json:"code"`     // the creation bytecode, hex
	ABI        json.RawMessage `json:"abi"`
	SourceHash string          `json:"sourceHash"` // identity.SourceHash of the source
	Compiler   Info            `json:"compiler"`
}

// Info says how the code was compiled.
type Info struct {
	Version  string         `json:"version"`
	Settings map[string]any `json:"settings"`
}

// ErrNoCompiler means the node has no Solidity compiler.
var ErrNoCompiler = errors.New("this node has no Solidity compiler (see -solc)")

// errSource means the compiler found errors in the source.
type errSource struct{ problems []string }

func (e errSource) Error() string {
	return "the source does not compile:\n" + strings.Join(e.problems, "\n")
}

// IsSourceError reports whether err is the compiler's verdict on the source,
// as opposed to a failure to run the compiler.
func IsSourceError(err error) bool {
	var e errSource
	return errors.As(err, &e)
}

// contract is one contract of a compiled source.
type contract struct {
	ABI  json.RawMessage
	Code string
}

// runner runs a compiler: --version, or --standard-json with input on stdin.
type runner func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

// standardJSON compiles source (one file) with settings, and returns its
// deployable contracts by name.
func standardJSON(ctx context.Context, run runner, source string, settings map[string]any) (map[string]contract, error) {
	all := map[string]any{"outputSelection": map[string]any{"*": map[string]any{"*": []string{"abi", "evm.bytecode.object"}}}}
	for k, v := range settings {
		if k != "outputSelection" {
			all[k] = v
		}
	}
	input, err := json.Marshal(map[string]any{
		"language": "Solidity",
		"sources":  map[string]any{"contract.sol": map[string]any{"content": source}},
		"settings": all,
	})
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, input, "--standard-json")
	if err != nil {
		return nil, err
	}
	// solc-js may print notices before the JSON.
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	var res struct {
		Errors []struct {
			Severity         string `json:"severity"`
			FormattedMessage string `json:"formattedMessage"`
		} `json:"errors"`
		Contracts map[string]map[string]struct {
			ABI json.RawMessage `json:"abi"`
			EVM struct {
				Bytecode struct {
					Object string `json:"object"`
				} `json:"bytecode"`
			} `json:"evm"`
		} `json:"contracts"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("compiler output: %w", err)
	}
	var problems []string
	for _, e := range res.Errors {
		if e.Severity == "error" {
			problems = append(problems, strings.TrimSpace(e.FormattedMessage))
		}
	}
	if len(problems) > 0 {
		return nil, errSource{problems}
	}
	contracts := map[string]contract{}
	for n, c := range res.Contracts["contract.sol"] {
		if c.EVM.Bytecode.Object != "" { // skip interfaces and abstract contracts
			contracts[n] = contract{ABI: c.ABI, Code: c.EVM.Bytecode.Object}
		}
	}
	return contracts, nil
}

// pick returns the contract named name, or the only one if name is empty.
func pick(contracts map[string]contract, name string) (string, contract, error) {
	names := make([]string, 0, len(contracts))
	for n := range contracts {
		names = append(names, n)
	}
	sort.Strings(names)
	if name == "" {
		if len(names) != 1 {
			return "", contract{}, fmt.Errorf("the source has %d contracts (%s): say which one", len(names), strings.Join(names, ", "))
		}
		name = names[0]
	}
	c, ok := contracts[name]
	if !ok {
		return "", contract{}, fmt.Errorf("no deployable contract %q in the source (it has: %s)", name, strings.Join(names, ", "))
	}
	return name, c, nil
}

// versionOf reads a compiler's version from its --version output.
func versionOf(out []byte) (string, error) {
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "Version: "); i >= 0 {
			line = line[i+len("Version: "):]
		}
		if line = strings.TrimSpace(line); len(line) > 0 && line[0] >= '0' && line[0] <= '9' {
			return line, nil
		}
	}
	return "", fmt.Errorf("compiler version: unexpected output %q", out)
}

var longVersion = regexp.MustCompile(`^(\d+\.\d+\.\d+)(-[0-9A-Za-z.]+)?\+commit\.[0-9a-f]+`)

// LongVersion is a compiler version without its platform: "0.8.38+commit.1835d247"
// for "0.8.38+commit.1835d247.Linux.g++". Builds of the same long version
// produce the same bytecode.
func LongVersion(version string) string {
	return longVersion.FindString(version)
}

// ShortVersion is the release of a compiler version: "0.8.38".
func ShortVersion(version string) string {
	if m := longVersion.FindStringSubmatch(version); m != nil && m[2] == "" {
		return m[1]
	}
	return ""
}

// Solc runs an external compiler: Command is the program and its arguments,
// for example ["solc"] or ["solcjs"].
type Solc struct {
	Command []string
	Timeout time.Duration // 0: one minute

	mu      sync.Mutex // one compilation at a time
	version string
}

// Version returns the compiler's version.
func (s *Solc) Version(ctx context.Context) (string, error) {
	if s == nil || len(s.Command) == 0 {
		return "", ErrNoCompiler
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.versionLocked(ctx)
}

func (s *Solc) versionLocked(ctx context.Context) (string, error) {
	if s.version != "" {
		return s.version, nil
	}
	out, err := s.run(ctx, nil, "--version")
	if err != nil {
		return "", err
	}
	if s.version, err = versionOf(out); err != nil {
		return "", err
	}
	return s.version, nil
}

// Compile compiles source (one file) with the node's Settings, and returns
// the contract named name, or the only contract in the file if name is empty.
func (s *Solc) Compile(ctx context.Context, source, name string) (Result, error) {
	if s == nil || len(s.Command) == 0 {
		return Result{}, ErrNoCompiler
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version, err := s.versionLocked(ctx)
	if err != nil {
		return Result{}, err
	}
	return compileResult(ctx, s.run, version, source, name)
}

func compileResult(ctx context.Context, run runner, version, source, name string) (Result, error) {
	contracts, err := standardJSON(ctx, run, source, Settings)
	if err != nil {
		return Result{}, err
	}
	name, c, err := pick(contracts, name)
	if err != nil {
		return Result{}, err
	}
	if _, err := identity.CodeHash(c.Code); err != nil {
		return Result{}, err
	}
	return Result{Contract: name, Code: c.Code, ABI: c.ABI, SourceHash: identity.SourceHash(source).Hex(),
		Compiler: Info{Version: version, Settings: Settings}}, nil
}

func (s *Solc) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return runCommand(ctx, s.Timeout, s.Command, stdin, args...)
}

func runCommand(ctx context.Context, timeout time.Duration, command []string, stdin []byte, args ...string) ([]byte, error) {
	if timeout == 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], append(append([]string{}, command[1:]...), args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var notFound *exec.Error
		if errors.As(err, &notFound) {
			return nil, ErrNoCompiler
		}
		return nil, fmt.Errorf("compiler: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
