package identity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestTransactionIDUnchanged: a transaction's id is its EIP-712 digest, the
// value ethers.js computes for the same transaction, whoever signs it.
func TestTransactionIDUnchanged(t *testing.T) {
	key, _ := crypto.HexToECDSA(testKeyHex)
	id, err := testTx(RootAddress(key)).Hash()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := id.Hex(), "0x87226137f43f8c410a76fcfbda7f06cb6d8b2f119517fff2b7d53d1cc145c150"; got != want {
		t.Fatalf("id %s, want %s", got, want)
	}
}

func TestTransactionChecks(t *testing.T) {
	root, _ := crypto.GenerateKey()
	agent := RootAddress(root)
	write := Secp256k1Signer{Key: root} // the root key signs transactions
	cases := map[string]Transaction{
		"unknown kind":          {Kind: "mint", Contract: "c", Agent: agent, Body: "{}"},
		"deploy with contract":  {Kind: KindDeploy, Contract: "c", Agent: agent, Body: `{"code":"60","abi":[],"node":"u","nodeKey":"0x0000000000000000000000000000000000000001"}`},
		"call without contract": {Kind: KindCall, Agent: agent, Body: `{"method":"m"}`},
		"call without method":   {Kind: KindCall, Contract: "c", Agent: agent, Body: `{}`},
		"join without key":      {Kind: KindJoin, Contract: "c", Agent: agent, Body: `{"node":"u"}`},
		"bad JSON":              {Kind: KindCall, Contract: "c", Agent: agent, Body: `{`},
	}
	for name, tx := range cases {
		stx, err := SignTx(tx, write)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stx.Verify(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Transaction{Kind: KindCall, Contract: "c", Agent: agent, Nonce: NewNonce(),
		Body: `{"method":"increment","values":{"amount":5}}`}
	stx, _ := SignTx(ok, write)
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
	reg := Registration{Agent: RootAddress(key), Node: "http://n", NodeKey: Address(node), Nonce: NewNonce()}
	sr, err := SignRegistration(reg, Secp256k1Signer{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, signer, err := sr.Verify(); err != nil || signer.Agent != reg.Agent {
		t.Fatal(signer, err)
	}
	sr.Registration.Node = "http://evil"
	if _, _, err := sr.Verify(); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered registration: %v", err)
	}
	// Only the root key registers: not the node, even under a grant.
	cert, _ := CertifyGrant(Grant{Agent: reg.Agent, NodeKey: Address(node), CodeHash: ethcommon.HexToHash("0x01"),
		Functions: []string{"m"}, Nonce: "n"}, Secp256k1Signer{Key: key})
	byNode, _ := SignRegistration(reg, Secp256k1Signer{Key: node, Cert: cert})
	if _, _, err := byNode.Verify(); err == nil || !strings.Contains(err.Error(), "signed by the root key") {
		t.Fatalf("registration by the node: %v", err)
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
