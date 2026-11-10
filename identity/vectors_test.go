package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/json"
	"flag"
	"math/big"
	"os"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

// vector is a message and the digest Go computes for it. The JavaScript
// client's test (js/test/keys.test.mjs) checks that it computes the same.
type vector struct {
	Type    string         `json:"type"`
	Message map[string]any `json:"message"`
	Digest  string         `json:"digest"`
}

type keyVector struct {
	PublicKey string `json:"publicKey"`
	Address   string `json:"address"`
}

func p256FromScalar(d int64) *ecdsa.PrivateKey {
	k := new(ecdsa.PrivateKey)
	k.Curve = elliptic.P256()
	k.D = big.NewInt(d)
	k.X, k.Y = k.Curve.ScalarBaseMult(k.D.Bytes()) //nolint:staticcheck // a fixed test key
	return k
}

// TestVectors keeps testdata/vectors.json, shared with the JavaScript test,
// equal to what Go computes. Run with -update after changing a type.
func TestVectors(t *testing.T) {
	secp, _ := crypto.HexToECDSA(testKeyHex)
	p256 := p256FromScalar(12345)
	agent := RootAddress(secp)
	node := ethcommon.HexToAddress("0x57E3339B18e36035656375139A5e66B19dc7D60d")

	tx := testTx(agent)
	reg := Registration{Agent: agent, Node: "https://ibc.example.com", NodeKey: node, Credential: []byte{0xca, 0xfe}, Nonce: "n1"}
	q := Query{Agent: agent, Action: "contract_read", Target: agent.Hex(), Contract: "0xabc",
		Body: `{"method":"count","values":{}}`, Time: 1789000000, Nonce: "n2"}
	grant := Grant{Agent: agent, NodeKey: node, CodeHash: crypto.Keccak256Hash([]byte("code")),
		Functions: []string{"post", "like"}, Nonce: "n3"}
	consent := Consent{Agent: agent, App: "https://app.example", AppKey: PublicKey(p256),
		CodeHash: crypto.Keccak256Hash([]byte("code")), Read: true, Functions: []string{"post"}, Nonce: "n4"}
	everything := Consent{Agent: agent, App: "https://ibc.example.com", AppKey: PublicKey(p256), Read: true,
		Functions: []string{}, Nonce: "n5"}

	var vectors []vector
	add := func(typ string, msg any, digest ethcommon.Hash, err error) {
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(msg)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		vectors = append(vectors, vector{Type: typ, Message: m, Digest: digest.Hex()})
	}
	h, err := tx.Hash()
	add("Transaction", tx, h, err)
	h, err = reg.Hash()
	add("Registration", reg, h, err)
	h, err = q.Hash()
	add("Query", q, h, err)
	h, err = grant.Hash()
	add("Grant", grant, h, err)
	h, err = consent.Hash()
	add("Consent", consent, h, err)
	h, err = everything.Hash()
	add("Consent", everything, h, err)
	device := Device{Agent: agent, App: "https://app.example", AppKey: PublicKey(p256), Nonce: "n6"}
	h, err = device.Hash()
	add("Device", device, h, err)
	items := []ethcommon.Hash{crypto.Keccak256Hash([]byte("one")), crypto.Keccak256Hash([]byte("two"))}
	h, err = BatchHash(items)
	add("Batch", map[string]any{"items": items}, h, err)

	keys := []keyVector{}
	for _, k := range []*ecdsa.PrivateKey{secp, p256} {
		a, err := AddressOf(PublicKey(k))
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, keyVector{PublicKey: hexutil.Encode(PublicKey(k)), Address: a.Hex()})
	}

	got, _ := json.MarshalIndent(map[string]any{"messages": vectors, "keys": keys}, "", "  ")
	got = append(got, '\n')
	const path = "testdata/vectors.json"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./identity -run TestVectors -update)", err)
	}
	if string(got) != string(want) {
		t.Fatal("testdata/vectors.json is out of date: run go test ./identity -run TestVectors -update")
	}
}
