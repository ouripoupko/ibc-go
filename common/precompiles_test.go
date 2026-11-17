package common

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"os"
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
)

// precompilesState runs testdata/Precompiles.sol.
func precompilesState(t *testing.T) *State {
	t.Helper()
	bin, err := os.ReadFile("../testdata/Precompiles.bin")
	if err != nil {
		t.Fatal(err)
	}
	abiJSON, err := os.ReadFile("../testdata/Precompiles.abi")
	if err != nil {
		t.Fatal(err)
	}
	doc := NewMemoryDocument(map[string]any{
		KeyPID: creator, KeyAddress: "http://alice.example",
		KeyCode: strings.TrimSpace(string(bin)), KeyABI: string(abiJSON),
	})
	s := NewState(doc, NewMemoryCollection(), rawdb.NewMemoryDatabase())
	if err := s.Run(creator, t0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return s
}

// word is n as a 32-byte big-endian word.
func word(n *big.Int) []byte { return ethcommon.LeftPadBytes(n.Bytes(), 32) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func dec(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad number %s", s)
	}
	return n
}

// The precompiles of Osaka are on: each is reached, and computes.
func TestPrecompiles(t *testing.T) {
	s := precompilesState(t)
	view := func(method string, values map[string]any) (any, error) {
		res, err := s.View("bob", method, values, t1)
		return res.Value, err
	}
	// run calls the precompile at addr (a short address: 0x01…, 0x100) with input.
	run := func(addr uint64, input []byte) ([]byte, error) {
		v, err := view("run", map[string]any{"precompile": ethcommon.BigToAddress(new(big.Int).SetUint64(addr)).Hex(), "input": hexutil.Encode(input)})
		if err != nil {
			return nil, err
		}
		return hexutil.MustDecode(v.(string)), nil
	}
	mustRun := func(name string, addr uint64, input []byte) []byte {
		t.Helper()
		out, err := run(addr, input)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	one := word(big.NewInt(1))

	// Solidity's sha256, ripemd160 and ecrecover.
	if v, err := view("hashSha256", map[string]any{"data": hexutil.Encode([]byte("abc"))}); err != nil ||
		v != "0xba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha256 = %v, %v", v, err)
	}
	if v, err := view("hashRipemd160", map[string]any{"data": hexutil.Encode([]byte("abc"))}); err != nil ||
		v != "0x8eb208f7e05d987a9b044a8e98c6b087f15a0bfc" {
		t.Errorf("ripemd160 = %v, %v", v, err)
	}
	key, _ := crypto.GenerateKey()
	digest := crypto.Keccak256([]byte("vote"))
	sig, _ := crypto.Sign(digest, key)
	v, err := view("recover", map[string]any{"digest": hexutil.Encode(digest), "v": int(sig[64]) + 27,
		"r": hexutil.Encode(sig[:32]), "s": hexutil.Encode(sig[32:64])})
	if err != nil || v != crypto.PubkeyToAddress(key.PublicKey).Hex() {
		t.Errorf("ecrecover = %v, %v; want %s", v, err, crypto.PubkeyToAddress(key.PublicKey).Hex())
	}

	// identity, and modexp: 3^5 mod 7 = 5.
	if out := mustRun("identity", 0x04, []byte("hello")); string(out) != "hello" {
		t.Errorf("identity = %q", out)
	}
	if out := mustRun("modexp", 0x05, cat(one, one, one, []byte{3, 5, 7})); !bytes.Equal(out, []byte{5}) {
		t.Errorf("modexp = %x", out)
	}

	// bn254, what a Groth16 verifier needs: G+G = 2·G, and e(G1, G2)·e(-G1, G2) = 1.
	g1 := cat(word(big.NewInt(1)), word(big.NewInt(2)))
	sum := mustRun("bn254 add", 0x06, cat(g1, g1))
	if double := mustRun("bn254 mul", 0x07, cat(g1, word(big.NewInt(2)))); len(sum) != 64 || !bytes.Equal(sum, double) {
		t.Errorf("bn254: G+G = %x, 2G = %x", sum, double)
	}
	p, _ := new(big.Int).SetString("30644e72e131a029b85045b68181585d97816a916871ca8d3c208c16d87cfd47", 16) // the field prime
	negG1 := cat(word(big.NewInt(1)), word(new(big.Int).Sub(p, big.NewInt(2))))
	g2 := cat( // x = x_re + x_im·i, y likewise; encoded imaginary part first
		word(dec(t, "11559732032986387107991004021392285783925812861821192530917403151452391805634")),
		word(dec(t, "10857046999023057135944570762232829481370756359578518086990519993285655852781")),
		word(dec(t, "4082367875863433681332203403145435568316851327593401208105741076214120093531")),
		word(dec(t, "8495653923123431417604973247489272438418190587263600148770280649306958101930")))
	if out := mustRun("bn254 pairing", 0x08, cat(g1, g2, negG1, g2)); !bytes.Equal(out, one) {
		t.Errorf("bn254 pairing e(G1,G2)·e(-G1,G2) = %x, want 1", out)
	}
	if out := mustRun("bn254 pairing", 0x08, cat(g1, g2, g1, g2)); !bytes.Equal(out, word(big.NewInt(0))) {
		t.Errorf("bn254 pairing e(G1,G2)² = %x, want 0", out)
	}

	// blake2f: 12 rounds of a zero state answer 64 bytes; a wrong length fails.
	if out := mustRun("blake2f", 0x09, cat([]byte{0, 0, 0, 12}, make([]byte, 64+128+16), []byte{1})); len(out) != 64 {
		t.Errorf("blake2f = %x", out)
	}
	if _, err := run(0x09, make([]byte, 212)); err == nil {
		t.Error("blake2f with a wrong input length did not fail")
	}

	// KZG point evaluation is there: it refuses a proof that is not one.
	// (An address with no precompile would succeed, with no output.)
	if _, err := run(0x0a, make([]byte, 192)); err == nil || !strings.Contains(err.Error(), "precompile failed") {
		t.Errorf("KZG point evaluation of a bad proof: %v", err)
	}

	// BLS12-381: the point at infinity plus itself is itself (128 bytes).
	if out := mustRun("bls12-381 G1 add", 0x0b, make([]byte, 256)); len(out) != 128 || !bytes.Equal(out, make([]byte, 128)) {
		t.Errorf("bls12-381 G1 add = %x", out)
	}

	// P-256 verification (0x100), the passkeys' curve: 1 for a good
	// signature, nothing for a bad one.
	pk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	h := sha256.Sum256([]byte("vote"))
	r, ss, _ := ecdsa.Sign(rand.Reader, pk, h[:])
	in := cat(h[:], word(r), word(ss), word(pk.X), word(pk.Y))
	if out := mustRun("p256verify", 0x100, in); !bytes.Equal(out, one) {
		t.Errorf("p256verify of a good signature = %x", out)
	}
	in[40] ^= 1
	if out := mustRun("p256verify", 0x100, in); len(out) != 0 {
		t.Errorf("p256verify of a bad signature = %x", out)
	}

	// Past the precompiles: an address with no code, a call that does nothing.
	if out := mustRun("empty address", 0x12, []byte("x")); len(out) != 0 {
		t.Errorf("0x12 = %s", hex.EncodeToString(out))
	}
}
