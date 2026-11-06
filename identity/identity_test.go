package identity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// A fixed key, so the digest below can be cross-checked with other EIP-712
// implementations (ethers.js produces the same values).
const testKeyHex = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func testTx(agent ethcommon.Address) Transaction {
	return Transaction{
		Contract: "",
		Kind:     KindDeploy,
		Agent:    agent,
		Nonce:    "00112233445566778899aabbccddeeff",
		Body:     `{"code":"6080","abi":[],"node":"http://localhost:2611","nodeKey":"0x0000000000000000000000000000000000000001"}`,
	}
}

func TestSignAndVerify(t *testing.T) {
	key, _ := crypto.HexToECDSA(testKeyHex)
	agent := Address(key)
	stx, err := Sign(testTx(agent), key)
	if err != nil {
		t.Fatal(err)
	}
	id, err := stx.Verify()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := stx.Tx.Hash()
	if id != want {
		t.Fatalf("id %s, want %s", id, want)
	}
	t.Logf("agent %s digest %s signature %x", agent.Hex(), id.Hex(), []byte(stx.Signature))

	// v = 0/1 is accepted as well as 27/28.
	low := append([]byte(nil), stx.Signature...)
	low[64] -= 27
	if _, err := (SignedTx{Tx: stx.Tx, Signature: low}).Verify(); err != nil {
		t.Fatalf("v=0/1: %v", err)
	}

	tampered := stx
	tampered.Tx.Nonce = "other"
	if _, err := tampered.Verify(); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered transaction: %v", err)
	}
	other, _ := crypto.GenerateKey()
	impostor := stx
	impostor.Tx.Agent = Address(other)
	if _, err := impostor.Verify(); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong agent: %v", err)
	}
}

func TestTransactionChecks(t *testing.T) {
	key, _ := crypto.GenerateKey()
	agent := Address(key)
	cases := map[string]Transaction{
		"unknown kind":          {Kind: "mint", Contract: "c", Agent: agent, Body: "{}"},
		"deploy with contract":  {Kind: KindDeploy, Contract: "c", Agent: agent, Body: `{"code":"60","abi":[],"node":"u","nodeKey":"0x0000000000000000000000000000000000000001"}`},
		"call without contract": {Kind: KindCall, Agent: agent, Body: `{"method":"m"}`},
		"call without method":   {Kind: KindCall, Contract: "c", Agent: agent, Body: `{}`},
		"join without key":      {Kind: KindJoin, Contract: "c", Agent: agent, Body: `{"node":"u"}`},
		"bad JSON":              {Kind: KindCall, Contract: "c", Agent: agent, Body: `{`},
	}
	for name, tx := range cases {
		stx, err := Sign(tx, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stx.Verify(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Transaction{Kind: KindCall, Contract: "c", Agent: agent, Nonce: NewNonce(),
		Body: `{"method":"increment","values":{"amount":5}}`}
	stx, _ := Sign(ok, key)
	if _, err := stx.Verify(); err != nil {
		t.Fatal(err)
	}
	body, _ := ok.ParseBody()
	if v := body.(*CallBody).Values["amount"]; v.(interface{ String() string }).String() != "5" {
		t.Fatalf("amount = %v (%T)", v, v)
	}
}

func TestRegistration(t *testing.T) {
	key, _ := crypto.GenerateKey()
	node, _ := crypto.GenerateKey()
	reg := Registration{Agent: Address(key), Node: "http://n", NodeKey: Address(node), Nonce: NewNonce()}
	sr, err := SignRegistration(reg, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sr.Verify(); err != nil {
		t.Fatal(err)
	}
	sr.Registration.Node = "http://evil"
	if _, err := sr.Verify(); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered registration: %v", err)
	}
}

func TestNodeKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodekey")
	k1, err := LoadOrCreateNodeKey(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("node key file mode %v, want 0600", info.Mode().Perm())
	}
	k2, err := LoadOrCreateNodeKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if Address(k1) != Address(k2) {
		t.Fatal("node key changed on reload")
	}
}
