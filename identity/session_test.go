package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func newSession(t *testing.T, agentKey *ecdsa.PrivateKey) (SignedSession, *ecdsa.PrivateKey) {
	t.Helper()
	sessionKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := sessionKey.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	ss, err := SignSession(Session{Agent: Address(agentKey), NodeKey: Address(agentKey), Key: pub,
		Scope: SessionScope, Expires: 1789000000, Nonce: NewNonce()}, agentKey)
	if err != nil {
		t.Fatal(err)
	}
	return ss, sessionKey
}

func TestSessionQuery(t *testing.T) {
	agentKey, _ := crypto.HexToECDSA(testKeyHex)
	ss, sessionKey := newSession(t, agentKey)
	q := Query{Agent: Address(agentKey), Action: "contract_read", Target: Address(agentKey).Hex(), Contract: "0xabc",
		Body: `{"method":"count","values":{}}`, Time: 1788990000, Nonce: NewNonce()}
	sq, err := SignWithSession(q, ss, sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sq.Verify(); err != nil {
		t.Fatalf("session query: %v", err)
	}

	fail := func(what string, sq SignedQuery, want string) {
		t.Helper()
		if _, err := sq.Verify(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", what, err, want)
		}
	}
	changed := sq
	changed.Query.Body = `{"method":"secret","values":{}}`
	fail("changed query", changed, "session key")

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	byOther, _ := SignWithSession(q, ss, other)
	fail("signed by another session key", byOther, "session key")

	stranger, _ := crypto.GenerateKey()
	asStranger := sq
	asStranger.Query.Agent = Address(stranger)
	fail("session of another agent", asStranger, "another agent")

	forged := ss
	forged.Session.Expires++
	withForged, _ := SignWithSession(q, forged, sessionKey)
	fail("session changed after signing", withForged, "signature")

	write := ss.Session
	write.Scope = "write"
	writeSession, _ := SignSession(write, agentKey)
	withWrite, _ := SignWithSession(q, writeSession, sessionKey)
	fail("a session with another scope", withWrite, "only \"read\"")
}
