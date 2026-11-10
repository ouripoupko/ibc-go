package identity

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"os"
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// keys is one agent's keys: a passkey root, and its node's key with a grant
// for two functions of one contract code.
type keys struct {
	agent    ethcommon.Address
	root     WebAuthnSigner
	nodeKey  *ecdsa.PrivateKey
	node     Secp256k1Signer // the node, signing under the grant
	codeHash ethcommon.Hash
}

func newKeys(t *testing.T) keys {
	t.Helper()
	rootKey, _ := NewP256Key()
	agent, err := AddressOf(PublicKey(rootKey))
	if err != nil {
		t.Fatal(err)
	}
	root := WebAuthnSigner{Key: rootKey, RPID: "node.example"}
	nodeKey, _ := crypto.GenerateKey()
	codeHash := crypto.Keccak256Hash([]byte("chat code"))
	cert, err := CertifyGrant(Grant{Agent: agent, NodeKey: Address(nodeKey), CodeHash: codeHash,
		Functions: []string{"post", "like"}, Nonce: NewNonce()}, root)
	if err != nil {
		t.Fatal(err)
	}
	return keys{agent: agent, root: root, nodeKey: nodeKey, node: Secp256k1Signer{Key: nodeKey, Cert: cert}, codeHash: codeHash}
}

func sign(t *testing.T, s KeySigner, digest ethcommon.Hash) Envelope {
	t.Helper()
	env, err := s.SignDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

var digest = crypto.Keccak256Hash([]byte("a message"))

func call(agent ethcommon.Address, method string) Transaction {
	return Transaction{Contract: "0xchat", Kind: KindCall, Agent: agent, Nonce: NewNonce(),
		Body: `{"method":"` + method + `","values":{}}`}
}

// An address is the keccak256 of the public key's coordinates, for both
// curves: secp256k1 addresses are Ethereum's, unchanged.
func TestAddressOf(t *testing.T) {
	key, _ := crypto.HexToECDSA(testKeyHex)
	addr, err := AddressOf(PublicKey(key))
	if err != nil || addr != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatalf("secp256k1 address %s, want %s (%v)", addr.Hex(), crypto.PubkeyToAddress(key.PublicKey).Hex(), err)
	}
	p, _ := NewP256Key()
	pub := PublicKey(p)
	addr, _ = AddressOf(pub)
	if addr != ethcommon.BytesToAddress(crypto.Keccak256(pub[1:])[12:]) {
		t.Fatal("p256 address")
	}
	if _, err := AddressOf(pub[1:]); err == nil {
		t.Fatal("a 64-byte key was accepted")
	}
}

// Each scheme verifies its own signatures and refuses a changed message.
func TestSchemes(t *testing.T) {
	secp, _ := crypto.GenerateKey()
	p, _ := NewP256Key()
	w, _ := NewP256Key()
	for name, s := range map[string]KeySigner{
		SchemeSecp256k1: Secp256k1Signer{Key: secp},
		SchemeP256:      P256Signer{Key: p},
		SchemeWebAuthn:  WebAuthnSigner{Key: w, RPID: "node.example"},
	} {
		env := sign(t, s, digest)
		signer, err := Verify(digest, env, ForRoot)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if want, _ := AddressOf(signer.Key); signer.Agent != want {
			t.Errorf("%s: agent %s", name, signer.Agent.Hex())
		}
		if _, err := Verify(crypto.Keccak256Hash([]byte("another")), env, ForRoot); err == nil {
			t.Errorf("%s: a signature of another message was accepted", name)
		}
	}
}

// The root key signs every kind of transaction.
func TestRootSignsTransactions(t *testing.T) {
	k := newKeys(t)
	for _, tx := range []Transaction{
		call(k.agent, "setTopic"),
		{Kind: KindJoin, Contract: "0xchat", Agent: k.agent, Nonce: "n", Body: `{"node":"n","nodeKey":"0x0000000000000000000000000000000000000001"}`},
		{Kind: KindLeave, Contract: "0xchat", Agent: k.agent, Nonce: "n", Body: `{"member":"` + k.agent.Hex() + `"}`},
	} {
		stx, err := SignTx(tx, k.root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stx.Verify(); err != nil || stx.Grant() != nil {
			t.Errorf("%s by the root: %v", tx.Kind, err)
		}
	}
	// Another key signing as the agent is refused.
	other, _ := NewP256Key()
	stx, _ := SignTx(call(k.agent, "post"), P256Signer{Key: other})
	if _, err := stx.Verify(); err != ErrBadSignature {
		t.Errorf("another key: %v", err)
	}
}

// The node signs calls under a grant: only the functions it names.
func TestNodeGrant(t *testing.T) {
	k := newKeys(t)
	stx, err := SignTx(call(k.agent, "post"), k.node)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stx.Verify(); err != nil {
		t.Fatalf("node signs post: %v", err)
	}
	if g := stx.Grant(); g == nil || g.CodeHash != k.codeHash {
		t.Fatalf("grant = %v", g)
	}

	fail := func(name string, stx SignedTx, want string) {
		t.Helper()
		if _, err := stx.Verify(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	setTopic, _ := SignTx(call(k.agent, "setTopic"), k.node)
	fail("a function the grant does not name", setTopic, "does not let the node call setTopic")
	join, _ := SignTx(Transaction{Kind: KindJoin, Contract: "0xchat", Agent: k.agent, Nonce: "n",
		Body: `{"node":"n","nodeKey":"0x0000000000000000000000000000000000000001"}`}, k.node)
	fail("a join", join, "a node signs only calls")

	otherNode, _ := crypto.GenerateKey()
	byOther, _ := SignTx(call(k.agent, "post"), Secp256k1Signer{Key: otherNode, Cert: k.node.Cert})
	fail("another node with this grant", byOther, "not by the node the grant names")

	other := newKeys(t)
	forged, _ := CertifyGrant(Grant{Agent: k.agent, NodeKey: Address(k.nodeKey), CodeHash: k.codeHash,
		Functions: []string{"transfer"}, Nonce: "n"}, other.root)
	byForged, _ := SignTx(call(k.agent, "transfer"), Secp256k1Signer{Key: k.nodeKey, Cert: forged})
	fail("a grant signed by another agent's root", byForged, "not signed by the agent's root key")

	changed := *k.node.Cert
	changed.Grant.Functions = []string{"post", "like", "transfer"}
	byChanged, _ := SignTx(call(k.agent, "transfer"), Secp256k1Signer{Key: k.nodeKey, Cert: &changed})
	fail("a grant changed after signing", byChanged, "grant")

	empty, _ := CertifyGrant(Grant{Agent: k.agent, NodeKey: Address(k.nodeKey), CodeHash: k.codeHash, Nonce: "n"}, k.root)
	byEmpty, _ := SignTx(call(k.agent, "post"), Secp256k1Signer{Key: k.nodeKey, Cert: empty})
	fail("a grant with no functions", byEmpty, "at least one function")

	// A grant allows transactions only.
	if _, err := Verify(digest, sign(t, k.node, digest), ForQuery); err == nil {
		t.Error("a grant was accepted for a query")
	}
	if _, err := Verify(digest, sign(t, k.node, digest), ForRoot); err == nil {
		t.Error("a grant was accepted for a root signature")
	}
	if err := VerifyGrant(k.node.Cert); err != nil {
		t.Errorf("VerifyGrant: %v", err)
	}
}

func TestConsent(t *testing.T) {
	k := newKeys(t)
	appKey, _ := NewP256Key()
	c := Consent{Agent: k.agent, App: "https://wishes.example", AppKey: PublicKey(appKey), CodeHash: k.codeHash,
		Read: true, Functions: []string{"post"}, Nonce: "n"}
	sc, err := SignConsent(c, k.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Verify(); err != nil {
		t.Fatal(err)
	}
	byApp, _ := SignConsent(c, P256Signer{Key: appKey})
	if err := byApp.Verify(); err != ErrBadSignature {
		t.Errorf("consent signed by the app: %v", err)
	}
	sc.Consent.Functions = append(sc.Consent.Functions, "transfer")
	if err := sc.Verify(); err == nil {
		t.Error("a changed consent was accepted")
	}
}

func TestWebAuthn(t *testing.T) {
	k := newKeys(t)
	fail := func(name string, env Envelope, want string) {
		t.Helper()
		if _, err := Verify(digest, env, ForRoot); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	noUser := k.root
	noUser.Flags = 0x04 // verified, but not present
	fail("no user presence", sign(t, noUser, digest), "did not approve")

	env := sign(t, k.root, digest)
	env.ClientDataJSON = strings.Replace(env.ClientDataJSON, "webauthn.get", "webauthn.create", 1)
	fail("not an assertion", env, "not an assertion")

	fail("challenge of another message", sign(t, k.root, crypto.Keccak256Hash([]byte("other"))), "for another message")

	env = sign(t, k.root, digest)
	env.AuthenticatorData[33]++ // the signature counter
	fail("changed authenticator data", env, ErrBadSignature.Error())
}

func TestBatch(t *testing.T) {
	k := newKeys(t)
	d1, d2 := crypto.Keccak256Hash([]byte("one")), crypto.Keccak256Hash([]byte("two"))
	envs, err := SignBatch(k.root, d1, d2)
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range []ethcommon.Hash{d1, d2} {
		if _, err := Verify(d, envs[i], ForRoot); err != nil {
			t.Errorf("batch item %d: %v", i, err)
		}
	}
	if _, err := Verify(crypto.Keccak256Hash([]byte("three")), envs[0], ForRoot); err == nil ||
		!strings.Contains(err.Error(), "not in its batch") {
		t.Errorf("a message outside the batch: %v", err)
	}
	changed := envs[0]
	changed.Batch = []ethcommon.Hash{d1}
	if _, err := Verify(d1, changed, ForRoot); err == nil {
		t.Error("a changed batch list was accepted")
	}
}

// Blocks carry signed transactions as their JSON text: it round-trips through
// RLP, so every partner computes the same block hash.
func TestSignedTxRLP(t *testing.T) {
	k := newKeys(t)
	for _, s := range []KeySigner{k.root, k.node} {
		stx, err := SignTx(Transaction{Contract: "0xchat", Kind: KindCall, Agent: k.agent, Nonce: "n",
			Body: `{"method":"post","values":{"v":"<1>"}}`}, s)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := rlp.EncodeToBytes([]SignedTx{stx})
		if err != nil {
			t.Fatal(err)
		}
		var dec []SignedTx
		if err := rlp.DecodeBytes(enc, &dec); err != nil {
			t.Fatal(err)
		}
		again, _ := rlp.EncodeToBytes(dec)
		if !bytes.Equal(enc, again) {
			t.Fatal("the encoding changed after a round trip")
		}
		if _, err := dec[0].Verify(); err != nil {
			t.Fatal(err)
		}
	}
}

// A registration signed by a real passkey in Chrome (captured by the browser
// test's tools): the node reads the browser's WebAuthn format.
func TestChromePasskey(t *testing.T) {
	raw, err := os.ReadFile("testdata/webauthn-chrome.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Registration Registration `json:"registration"`
		Digest       string       `json:"digest"`
		Signature    Envelope     `json:"signature"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if h, _ := v.Registration.Hash(); h.Hex() != v.Digest {
		t.Fatalf("digest %s, the browser signed %s", h.Hex(), v.Digest)
	}
	sr := SignedRegistration{Registration: v.Registration, Signature: v.Signature}
	if _, signer, err := sr.Verify(); err != nil || signer.Agent != v.Registration.Agent {
		t.Fatalf("chrome passkey: %v %v", signer, err)
	}
	sr.Registration.Nonce = "changed"
	if _, _, err := sr.Verify(); err == nil {
		t.Fatal("a changed registration was accepted")
	}
}
