package identity

import (
	"errors"
	"fmt"
	"regexp"
	"sort"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Sources is a contract's Solidity source: its files, by path. Files import
// each other by these paths (solc resolves "./A.sol" against the importing
// file's path); a contract compiles from its files alone.
type Sources map[string]string

// MaxSourceFiles is the most files a contract's source may have.
const MaxSourceFiles = 256

// sourcePath is a file's path: segments of letters, digits, '.', '_' and
// '-', none starting with '.', separated by '/', ending in ".sol". ASCII, so
// that every implementation sorts paths the same way.
var sourcePath = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*\.sol$`)

// Validate checks that the source has files, each with a valid path and
// some content.
func (s Sources) Validate() error {
	if len(s) == 0 {
		return errors.New("the source has no files")
	}
	if len(s) > MaxSourceFiles {
		return fmt.Errorf("the source has %d files, at most %d are allowed", len(s), MaxSourceFiles)
	}
	for path, content := range s {
		if !sourcePath.MatchString(path) {
			return fmt.Errorf("source file path %q: use relative paths of letters, digits, '.', '_', '-' and '/', ending in .sol", path)
		}
		if content == "" {
			return fmt.Errorf("source file %s is empty", path)
		}
	}
	return nil
}

// Paths returns the source's paths, sorted (byte order).
func (s Sources) Paths() []string {
	paths := make([]string, 0, len(s))
	for p := range s {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// SourcesFrom reads a source decoded from JSON (a map of strings) or held
// as Sources, and validates it.
func SourcesFrom(v any) (Sources, error) {
	var s Sources
	switch m := v.(type) {
	case Sources:
		s = m
	case map[string]string:
		s = m
	case map[string]any:
		s = make(Sources, len(m))
		for path, content := range m {
			text, ok := content.(string)
			if !ok {
				return nil, fmt.Errorf("source file %s is not text", path)
			}
			s[path] = text
		}
	default:
		return nil, errors.New("the source is not a map of files")
	}
	return s, s.Validate()
}

// SourceHash is a contract's source hash, as joins, grants and consents name
// it: keccak256 over the files sorted by path, each as keccak256(path) ‖
// keccak256(content) (UTF-8 bytes). Any order of the same files gives the same
// hash; a change to any path or content changes it.
func SourceHash(s Sources) ethcommon.Hash {
	buf := make([]byte, 0, 64*len(s))
	for _, path := range s.Paths() {
		buf = append(buf, crypto.Keccak256([]byte(path))...)
		buf = append(buf, crypto.Keccak256([]byte(s[path]))...)
	}
	return crypto.Keccak256Hash(buf)
}
