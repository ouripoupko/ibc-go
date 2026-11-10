// Package compiler compiles Solidity source on the node, so that apps send
// source code and the node, not the app, produces the bytecode a user signs.
// It runs an external compiler that speaks solc's standard JSON: the native
// solc, or solc-js (solcjs). The version and settings go into the deploy
// transaction, so anyone can compile the source again and compare.
package compiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ouripoupko/ibc-go/identity"
)

// Settings are the compiler settings the node uses, fixed so that the same
// source always gives the same bytecode.
var Settings = map[string]any{
	"optimizer": map[string]any{"enabled": true, "runs": 200},
}

// Result is a compiled contract.
type Result struct {
	Contract string          `json:"contract"` // the contract's name in the source
	Code     string          `json:"code"`     // the creation bytecode, hex
	ABI      json.RawMessage `json:"abi"`
	CodeHash string          `json:"codeHash"` // identity.CodeHash of Code
	Compiler Info            `json:"compiler"`
}

// Info says how the code was compiled.
type Info struct {
	Version  string         `json:"version"`
	Settings map[string]any `json:"settings"`
}

// ErrNoCompiler means the node has no Solidity compiler.
var ErrNoCompiler = errors.New("this node has no Solidity compiler (see -solc)")

// Solc runs an external compiler: Command is the program and its arguments,
// for example ["solc"] or ["npx", "-y", "-p", "solc@0.8.38", "solcjs"].
type Solc struct {
	Command []string
	Timeout time.Duration // 0: one minute

	mu      sync.Mutex // one compilation at a time
	version string
}

// Version returns the compiler's version.
func (s *Solc) Version(ctx context.Context) (string, error) {
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
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "Version: "); i >= 0 {
			line = line[i+len("Version: "):]
		}
		if line = strings.TrimSpace(line); len(line) > 0 && line[0] >= '0' && line[0] <= '9' {
			s.version = line
			return line, nil
		}
	}
	return "", fmt.Errorf("compiler version: unexpected output %q", out)
}

// Compile compiles source (one file) and returns the contract named name,
// or the only contract in the file if name is empty.
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
	settings := map[string]any{"outputSelection": map[string]any{"*": map[string]any{"*": []string{"abi", "evm.bytecode.object"}}}}
	for k, v := range Settings {
		settings[k] = v
	}
	input, err := json.Marshal(map[string]any{
		"language": "Solidity",
		"sources":  map[string]any{"contract.sol": map[string]any{"content": source}},
		"settings": settings,
	})
	if err != nil {
		return Result{}, err
	}
	out, err := s.run(ctx, input, "--standard-json")
	if err != nil {
		return Result{}, err
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
		return Result{}, fmt.Errorf("compiler output: %w", err)
	}
	var problems []string
	for _, e := range res.Errors {
		if e.Severity == "error" {
			problems = append(problems, strings.TrimSpace(e.FormattedMessage))
		}
	}
	if len(problems) > 0 {
		return Result{}, fmt.Errorf("the source does not compile:\n%s", strings.Join(problems, "\n"))
	}
	contracts := res.Contracts["contract.sol"]
	var names []string
	for n, c := range contracts {
		if c.EVM.Bytecode.Object != "" { // skip interfaces and abstract contracts
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if name == "" {
		if len(names) != 1 {
			return Result{}, fmt.Errorf("the source has %d contracts (%s): say which one", len(names), strings.Join(names, ", "))
		}
		name = names[0]
	}
	c, ok := contracts[name]
	if !ok || c.EVM.Bytecode.Object == "" {
		return Result{}, fmt.Errorf("no deployable contract %q in the source (it has: %s)", name, strings.Join(names, ", "))
	}
	hash, err := identity.CodeHash(c.EVM.Bytecode.Object)
	if err != nil {
		return Result{}, err
	}
	return Result{Contract: name, Code: c.EVM.Bytecode.Object, ABI: c.ABI, CodeHash: hash.Hex(),
		Compiler: Info{Version: version, Settings: Settings}}, nil
}

func (s *Solc) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Command[0], append(append([]string{}, s.Command[1:]...), args...)...)
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
