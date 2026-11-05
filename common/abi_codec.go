package common

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// This file converts between JSON-like values (what arrives in a record's
// message.values and what is published as a reply) and the Go types that
// go-ethereum's accounts/abi package packs and unpacks.
//
// Input conventions:
//   - integers: JSON number, json.Number, Go integer, *big.Int, or a string
//     (decimal, or hex with 0x prefix). Use strings or json.Number for values
//     above 2^53; a float64 that cannot be represented exactly is rejected.
//   - address, bytes, bytesN: 0x-prefixed hex string.
//   - arrays / slices: JSON array. Tuples: JSON object keyed by component name.
//
// Output conventions:
//   - integers: json.Number (exact, serialised as a JSON number).
//   - address: checksummed hex string. bytes / bytesN: 0x-prefixed hex string.
//   - arrays / slices: []any. Tuples: map[string]any keyed by component name.

// packArguments encodes values (keyed by argument name) for the given
// arguments, in ABI order. Like a Python call with **kwargs, a missing or an
// unexpected argument is an error. An unnamed argument is addressed by its
// position as a decimal string ("0", "1", ...).
func packArguments(args abi.Arguments, values map[string]any) ([]byte, error) {
	ordered := make([]any, len(args))
	used := 0
	for i, arg := range args {
		key := argumentKey(arg, i)
		raw, ok := values[key]
		if !ok {
			return nil, fmt.Errorf("missing argument %q", key)
		}
		used++
		v, err := fromJSON(arg.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("argument %q: %w", key, err)
		}
		ordered[i] = v.Interface()
	}
	if used != len(values) {
		for key := range values {
			if !hasArgument(args, key) {
				return nil, fmt.Errorf("unexpected argument %q", key)
			}
		}
	}
	return args.Pack(ordered...)
}

func argumentKey(arg abi.Argument, index int) string {
	if arg.Name == "" {
		return strconv.Itoa(index)
	}
	return arg.Name
}

func hasArgument(args abi.Arguments, key string) bool {
	for i, arg := range args {
		if argumentKey(arg, i) == key {
			return true
		}
	}
	return false
}

// unpackOutputs decodes return data into a JSON-serialisable value: nil for no
// outputs, the value itself for one output, and a []any for several (as a
// Python tuple would be serialised).
func unpackOutputs(args abi.Arguments, data []byte) (any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	values, err := args.Unpack(data)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = toJSON(args[i].Type, reflect.ValueOf(v))
	}
	if len(out) == 1 {
		return out[0], nil
	}
	return out, nil
}

// fromJSON converts a JSON-like value into a reflect.Value of the exact Go type
// accounts/abi expects for t.
func fromJSON(t abi.Type, v any) (reflect.Value, error) {
	target := t.GetType()
	switch t.T {
	case abi.IntTy, abi.UintTy:
		n, err := toBigInt(v)
		if err != nil {
			return reflect.Value{}, err
		}
		if err := checkIntRange(t, n); err != nil {
			return reflect.Value{}, err
		}
		if target == reflect.TypeFor[*big.Int]() {
			return reflect.ValueOf(n), nil
		}
		out := reflect.New(target).Elem()
		if t.T == abi.IntTy {
			out.SetInt(n.Int64())
		} else {
			out.SetUint(n.Uint64())
		}
		return out, nil

	case abi.BoolTy:
		b, ok := v.(bool)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected bool, got %T", v)
		}
		return reflect.ValueOf(b), nil

	case abi.StringTy:
		s, ok := v.(string)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected string, got %T", v)
		}
		return reflect.ValueOf(s), nil

	case abi.AddressTy:
		switch a := v.(type) {
		case ethcommon.Address:
			return reflect.ValueOf(a), nil
		case string:
			if !ethcommon.IsHexAddress(a) {
				return reflect.Value{}, fmt.Errorf("invalid address %q", a)
			}
			return reflect.ValueOf(ethcommon.HexToAddress(a)), nil
		}
		return reflect.Value{}, fmt.Errorf("expected address string, got %T", v)

	case abi.BytesTy:
		b, err := toBytes(v)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(b), nil

	case abi.FixedBytesTy, abi.FunctionTy, abi.HashTy:
		b, err := toBytes(v)
		if err != nil {
			return reflect.Value{}, err
		}
		out := reflect.New(target).Elem()
		if len(b) != out.Len() {
			return reflect.Value{}, fmt.Errorf("expected %d bytes, got %d", out.Len(), len(b))
		}
		reflect.Copy(out, reflect.ValueOf(b))
		return out, nil

	case abi.SliceTy, abi.ArrayTy:
		items, ok := v.([]any)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected array, got %T", v)
		}
		var out reflect.Value
		if t.T == abi.SliceTy {
			out = reflect.MakeSlice(target, len(items), len(items))
		} else {
			if len(items) != t.Size {
				return reflect.Value{}, fmt.Errorf("expected %d elements, got %d", t.Size, len(items))
			}
			out = reflect.New(target).Elem()
		}
		for i, item := range items {
			elem, err := fromJSON(*t.Elem, item)
			if err != nil {
				return reflect.Value{}, fmt.Errorf("[%d]: %w", i, err)
			}
			out.Index(i).Set(elem)
		}
		return out, nil

	case abi.TupleTy:
		fields, ok := v.(map[string]any)
		if !ok {
			return reflect.Value{}, fmt.Errorf("expected object, got %T", v)
		}
		if len(fields) != len(t.TupleElems) {
			return reflect.Value{}, fmt.Errorf("expected %d fields, got %d", len(t.TupleElems), len(fields))
		}
		out := reflect.New(target).Elem()
		for i, elemType := range t.TupleElems {
			name := t.TupleRawNames[i]
			raw, ok := fields[name]
			if !ok {
				return reflect.Value{}, fmt.Errorf("missing field %q", name)
			}
			elem, err := fromJSON(*elemType, raw)
			if err != nil {
				return reflect.Value{}, fmt.Errorf("field %q: %w", name, err)
			}
			out.Field(i).Set(elem)
		}
		return out, nil
	}
	return reflect.Value{}, fmt.Errorf("unsupported ABI type %s", t.String())
}

// toJSON converts a value produced by accounts/abi into a JSON-serialisable value.
func toJSON(t abi.Type, v reflect.Value) any {
	switch t.T {
	case abi.IntTy, abi.UintTy:
		switch n := v.Interface().(type) {
		case *big.Int:
			return json.Number(n.String())
		}
		if t.T == abi.IntTy {
			return json.Number(strconv.FormatInt(v.Int(), 10))
		}
		return json.Number(strconv.FormatUint(v.Uint(), 10))
	case abi.BoolTy, abi.StringTy:
		return v.Interface()
	case abi.AddressTy:
		return v.Interface().(ethcommon.Address).Hex()
	case abi.BytesTy:
		return hexutil.Encode(v.Bytes())
	case abi.FixedBytesTy, abi.FunctionTy, abi.HashTy:
		b := make([]byte, v.Len())
		reflect.Copy(reflect.ValueOf(b), v)
		return hexutil.Encode(b)
	case abi.SliceTy, abi.ArrayTy:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = toJSON(*t.Elem, v.Index(i))
		}
		return out
	case abi.TupleTy:
		out := make(map[string]any, len(t.TupleElems))
		for i, elemType := range t.TupleElems {
			out[t.TupleRawNames[i]] = toJSON(*elemType, v.Field(i))
		}
		return out
	}
	return fmt.Sprint(v.Interface())
}

func toBigInt(v any) (*big.Int, error) {
	switch n := v.(type) {
	case *big.Int:
		return new(big.Int).Set(n), nil
	case json.Number:
		return parseBigInt(string(n))
	case string:
		return parseBigInt(n)
	case float64:
		if n != math.Trunc(n) || math.Abs(n) > 1<<53 {
			return nil, fmt.Errorf("number %v is not an exact integer; pass it as a string", n)
		}
		return big.NewInt(int64(n)), nil
	case float32:
		return toBigInt(float64(n))
	case int:
		return big.NewInt(int64(n)), nil
	case int8:
		return big.NewInt(int64(n)), nil
	case int16:
		return big.NewInt(int64(n)), nil
	case int32:
		return big.NewInt(int64(n)), nil
	case int64:
		return big.NewInt(n), nil
	case uint:
		return new(big.Int).SetUint64(uint64(n)), nil
	case uint8:
		return new(big.Int).SetUint64(uint64(n)), nil
	case uint16:
		return new(big.Int).SetUint64(uint64(n)), nil
	case uint32:
		return new(big.Int).SetUint64(uint64(n)), nil
	case uint64:
		return new(big.Int).SetUint64(n), nil
	}
	return nil, fmt.Errorf("expected integer, got %T", v)
}

func parseBigInt(s string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(strings.TrimSpace(s), 0)
	if !ok {
		return nil, fmt.Errorf("invalid integer %q", s)
	}
	return n, nil
}

func checkIntRange(t abi.Type, n *big.Int) error {
	if t.T == abi.UintTy {
		if n.Sign() < 0 || n.BitLen() > t.Size {
			return fmt.Errorf("%s out of range for uint%d", n, t.Size)
		}
		return nil
	}
	limit := new(big.Int).Lsh(big.NewInt(1), uint(t.Size-1)) // 2^(size-1)
	if n.Cmp(limit) >= 0 || n.Cmp(new(big.Int).Neg(limit)) < 0 {
		return fmt.Errorf("%s out of range for int%d", n, t.Size)
	}
	return nil
}

func toBytes(v any) ([]byte, error) {
	switch b := v.(type) {
	case []byte:
		return b, nil
	case string:
		return decodeHex(b)
	}
	return nil, fmt.Errorf("expected 0x-prefixed hex string, got %T", v)
}

// decodeHex decodes a hex string, with or without a 0x prefix.
func decodeHex(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	return hex.DecodeString(s)
}
