package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// ABISet is the ABIs of a source's contracts, taken together: what a user's
// node knows about an agreement's functions, to encode calls by name and to
// decode results and reverts. Consensus never uses it: partners execute
// calldata.
//
// In a source, a selector means one function: the same signature, the same
// outputs and the same kind (a read, or a transaction) in every contract that
// has it; and one custom error. NewABISet refuses a source that breaks this,
// so a call's data alone says which function it calls, whatever contract runs
// it, and a grant's signature cannot be mistaken for another function.
type ABISet struct {
	functions  map[string]*Function // by signature
	bySelector map[[4]byte]*Function
	byName     map[string][]*Function
	errors     map[[4]byte]abi.Error
	contracts  map[string]abi.ABI
}

// Function is a function of a source, and the contracts that have it.
type Function struct {
	abi.Method
	Contracts []string
}

// NewABISet reads the ABIs of a source's contracts, by name. A contract whose
// ABI go-ethereum cannot read (a library with storage parameters) is left
// out: it cannot be called directly.
func NewABISet(abis map[string]json.RawMessage) (*ABISet, error) {
	s := &ABISet{functions: map[string]*Function{}, bySelector: map[[4]byte]*Function{},
		byName: map[string][]*Function{}, errors: map[[4]byte]abi.Error{}, contracts: map[string]abi.ABI{}}
	names := make([]string, 0, len(abis))
	for name := range abis {
		names = append(names, name)
	}
	sort.Strings(names)
	errorOwner := map[[4]byte]string{}
	for _, name := range names {
		parsed, err := abi.JSON(bytes.NewReader(abis[name]))
		if err != nil {
			continue
		}
		s.contracts[name] = parsed
		for _, m := range parsed.Methods {
			var sel [4]byte
			copy(sel[:], m.ID)
			if f, ok := s.bySelector[sel]; ok {
				if f.Sig != m.Sig {
					return nil, fmt.Errorf("%s.%s and %s.%s have the same selector %s: rename one",
						f.Contracts[0], f.Sig, name, m.Sig, hexutil.Encode(m.ID))
				}
				if outputs(f.Outputs) != outputs(m.Outputs) || f.IsConstant() != m.IsConstant() {
					return nil, fmt.Errorf("%s means different functions in %s and %s (outputs or view): make them the same, or rename one",
						m.Sig, f.Contracts[0], name)
				}
				f.Contracts = append(f.Contracts, name)
				continue
			}
			f := &Function{Method: m, Contracts: []string{name}}
			s.functions[m.Sig] = f
			s.bySelector[sel] = f
			s.byName[m.RawName] = append(s.byName[m.RawName], f)
		}
		for _, e := range parsed.Errors {
			var sel [4]byte
			copy(sel[:], e.ID[:4])
			if old, ok := s.errors[sel]; ok {
				if old.Sig != e.Sig {
					return nil, fmt.Errorf("errors %s (in %s) and %s (in %s) have the same selector: rename one",
						old.Sig, errorOwner[sel], e.Sig, name)
				}
				continue
			}
			s.errors[sel] = e
			errorOwner[sel] = name
		}
	}
	for _, fs := range s.byName {
		sort.Slice(fs, func(i, j int) bool { return fs[i].Sig < fs[j].Sig })
	}
	return s, nil
}

// outputs is the canonical text of a function's output types.
func outputs(args abi.Arguments) string {
	types := make([]string, len(args))
	for i, a := range args {
		types[i] = a.Type.String()
	}
	return strings.Join(types, ",")
}

// Contract returns a contract's ABI.
func (s *ABISet) Contract(name string) (abi.ABI, bool) {
	a, ok := s.contracts[name]
	return a, ok
}

// Function finds a function by its signature ("pay(address,uint256)"), or by
// its name if only one function of the source has that name.
func (s *ABISet) Function(nameOrSignature string) (*Function, error) {
	if strings.Contains(nameOrSignature, "(") {
		if f, ok := s.functions[nameOrSignature]; ok {
			return f, nil
		}
		return nil, fmt.Errorf("no function %s in the source", nameOrSignature)
	}
	fs := s.byName[nameOrSignature]
	switch len(fs) {
	case 0:
		return nil, fmt.Errorf("unknown method %q", nameOrSignature)
	case 1:
		return fs[0], nil
	}
	sigs := make([]string, len(fs))
	for i, f := range fs {
		sigs[i] = f.Sig
	}
	return nil, fmt.Errorf("the source has several functions named %s (%s): name it by its signature", nameOrSignature, strings.Join(sigs, ", "))
}

// BySelector returns the function a call's data calls, or nil.
func (s *ABISet) BySelector(data []byte) *Function {
	if len(data) < 4 {
		return nil
	}
	var sel [4]byte
	copy(sel[:], data)
	return s.bySelector[sel]
}

// Encode returns the calldata of a call to f with values, keyed by argument
// name.
func (f *Function) Encode(values map[string]any) ([]byte, error) {
	args, err := packArguments(f.Inputs, values)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.RawName, err)
	}
	return append(slices.Clone(f.ID), args...), nil
}

// Decode decodes what f returned: nil for no outputs, the value for one, and
// an array for several.
func (f *Function) Decode(ret []byte) (any, error) {
	v, err := unpackOutputs(f.Outputs, ret)
	if err != nil {
		return nil, fmt.Errorf("%s: decode output: %w", f.RawName, err)
	}
	return v, nil
}

// DecodeRevert says why a call reverted, from its revert data: the reason of
// Error(string), the code of Panic(uint256), or a custom error of the source
// with its arguments. Data it cannot read is returned as hex.
func (s *ABISet) DecodeRevert(data []byte) string {
	if reason, err := abi.UnpackRevert(data); err == nil {
		return reason
	}
	if len(data) >= 4 && s != nil {
		var sel [4]byte
		copy(sel[:], data)
		if e, ok := s.errors[sel]; ok {
			if values, err := e.Inputs.Unpack(data[4:]); err == nil {
				args := make([]string, len(values))
				for i, v := range values {
					b, _ := json.Marshal(toJSON(e.Inputs[i].Type, reflect.ValueOf(v)))
					args[i] = string(b)
				}
				return e.Name + "(" + strings.Join(args, ", ") + ")"
			}
		}
	}
	return hexutil.Encode(data)
}

// EncodeItem encodes values for one ABI item (a function, or a
// constructor): for a function, its calldata; for a constructor, its
// arguments, to append to the creation code.
func EncodeItem(item json.RawMessage, values map[string]any) ([]byte, error) {
	parsed, err := abi.JSON(bytes.NewReader(append(append([]byte("["), item...), ']')))
	if err != nil {
		return nil, fmt.Errorf("ABI item: %w", err)
	}
	for _, m := range parsed.Methods {
		return (&Function{Method: m}).Encode(values)
	}
	if parsed.Constructor.Type == abi.Constructor {
		return packArguments(parsed.Constructor.Inputs, values)
	}
	return nil, errors.New("the ABI item is not a function or a constructor")
}
