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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ouripoupko/ibc-go/common"
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
	Contract string          `json:"contract"` // the contract's name in the source: "path:Name"
	Code     string          `json:"code"`     // its creation bytecode, hex
	ABI      json.RawMessage `json:"abi"`      // its ABI
	// ABIs is the ABI of every contract in the source, by name ("path:Name"),
	// and Membership the membership functions the contract defines: with
	// Code, what a deploy carries, and a joining node checks.
	ABIs       map[string]json.RawMessage `json:"abis"`
	Membership []string                   `json:"membership"`
	SourceHash string                     `json:"sourceHash"` // identity.SourceHash of the source
	Compiler   Info                       `json:"compiler"`
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

// standardJSON compiles the source's files with settings, and returns their
// deployable contracts by "path:Name". The files are all the compiler gets:
// it runs with no import callback, in an empty directory, and a compilation
// that read any file not in the source is refused, so imports resolve only
// within the source.
func standardJSON(ctx context.Context, run runner, source identity.Sources, settings map[string]any) (map[string]contract, error) {
	if err := source.Validate(); err != nil {
		return nil, err
	}
	all := map[string]any{"outputSelection": map[string]any{"*": map[string]any{"*": []string{"abi", "evm.bytecode.object"}}}}
	for k, v := range settings {
		if k != "outputSelection" {
			all[k] = v
		}
	}
	files := make(map[string]any, len(source))
	for path, content := range source {
		files[path] = map[string]any{"content": content}
	}
	input, err := json.Marshal(map[string]any{
		"language": "Solidity",
		"sources":  files,
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
		Sources   map[string]json.RawMessage `json:"sources"`
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
	for path := range res.Sources {
		if _, ok := source[path]; !ok {
			return nil, errSource{[]string{fmt.Sprintf("the compiler read %s, which is not in the source: imports must name the source's own files", path)}}
		}
	}
	for path := range res.Contracts {
		if _, ok := source[path]; !ok {
			return nil, errSource{[]string{fmt.Sprintf("the compiler compiled %s, which is not in the source", path)}}
		}
	}
	contracts := map[string]contract{}
	for path, cs := range res.Contracts {
		for n, c := range cs {
			// Interfaces and abstract contracts have no code, but an ABI.
			contracts[path+":"+n] = contract{ABI: c.ABI, Code: c.EVM.Bytecode.Object}
		}
	}
	return contracts, nil
}

// describe returns what a deploy of the contract key carries besides its
// code: the ABIs of all the source's contracts, and the membership functions
// the contract defines. It refuses a source in which a selector means two
// functions (see common.ABISet), and a membership function that does not
// return bool.
func describe(contracts map[string]contract, key string) (map[string]json.RawMessage, []string, error) {
	abis := make(map[string]json.RawMessage, len(contracts))
	for k, c := range contracts {
		abis[k] = c.ABI
	}
	set, err := common.NewABISet(abis)
	if err != nil {
		return nil, nil, errSource{[]string{err.Error()}}
	}
	root, _ := set.Contract(key)
	membership := []string{}
	for _, name := range identity.MembershipFunctions {
		for _, m := range root.Methods {
			if m.Sig != identity.MembershipSignature(name) {
				continue
			}
			if len(m.Outputs) != 1 || m.Outputs[0].Type.String() != "bool" {
				return nil, nil, errSource{[]string{fmt.Sprintf("%s must return bool: it decides a membership change", m.Sig)}}
			}
			membership = append(membership, name)
		}
	}
	return abis, membership, nil
}

// pick returns the deployable contract that name names: "path:Name", or
// "Name" if only one file has a contract of that name; or the only one, if
// name is empty. It returns the contract's key, "path:Name".
func pick(contracts map[string]contract, name string) (string, contract, error) {
	keys := make([]string, 0, len(contracts))
	for k, c := range contracts {
		if c.Code != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	shortName := func(key string) string { return key[strings.LastIndex(key, ":")+1:] }
	if name == "" {
		if len(keys) != 1 {
			return "", contract{}, fmt.Errorf("the source has %d deployable contracts (%s): say which one", len(keys), strings.Join(keys, ", "))
		}
		return keys[0], contracts[keys[0]], nil
	}
	if c, ok := contracts[name]; ok && c.Code != "" {
		return name, c, nil
	}
	var found []string
	for _, k := range keys {
		if shortName(k) == name {
			found = append(found, k)
		}
	}
	switch len(found) {
	case 1:
		return found[0], contracts[found[0]], nil
	case 0:
		return "", contract{}, fmt.Errorf("no deployable contract %q in the source (it has: %s)", name, strings.Join(keys, ", "))
	default:
		return "", contract{}, fmt.Errorf("%d files have a contract %q (%s): name it as path:Name", len(found), name, strings.Join(found, ", "))
	}
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

// Compile compiles source with the node's Settings, and returns the contract
// that name names ("Name" or "path:Name"), or the only deployable contract if
// name is empty.
func (s *Solc) Compile(ctx context.Context, source identity.Sources, name string) (Result, error) {
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

func compileResult(ctx context.Context, run runner, version string, source identity.Sources, name string) (Result, error) {
	contracts, err := standardJSON(ctx, run, source, Settings)
	if err != nil {
		return Result{}, err
	}
	key, c, err := pick(contracts, name)
	if err != nil {
		return Result{}, err
	}
	if _, err := identity.CodeHash(c.Code); err != nil {
		return Result{}, err
	}
	abis, membership, err := describe(contracts, key)
	if err != nil {
		return Result{}, err
	}
	return Result{Contract: key, Code: c.Code, ABI: c.ABI, ABIs: abis, Membership: membership,
		SourceHash: identity.SourceHash(source).Hex(), Compiler: Info{Version: version, Settings: Settings}}, nil
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
	// The compiler runs in an empty directory, so that one that reads
	// imports from its directory finds nothing there; a program given by a
	// relative path is found from here first.
	prog := command[0]
	if strings.ContainsRune(prog, filepath.Separator) && !filepath.IsAbs(prog) {
		abs, err := filepath.Abs(prog)
		if err != nil {
			return nil, err
		}
		prog = abs
	}
	dir, err := os.MkdirTemp("", "ibc-solc-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, prog, append(append([]string{}, command[1:]...), args...)...)
	cmd.Dir = dir
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
