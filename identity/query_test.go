package identity

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// TestQueryMatchesEthers checks the Query digest and signature against the
// values ethers.js's signTypedData gives for the same key and query.
func TestQueryMatchesEthers(t *testing.T) {
	key, _ := crypto.HexToECDSA(testKeyHex)
	agent := Address(key)
	q := Query{Agent: agent, Action: "contract_read", Target: agent.Hex(), Contract: "0xabc",
		Body: `{"method":"count","values":{}}`, Time: 1789000000, Nonce: "00112233445566778899aabbccddeeff"}
	sq, err := SignQuery(q, key)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := sq.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := digest.Hex(), "0x5dea9ed942bfaef6da5ee354d5e2fa93cac3263edbbc902a3effacb8c61ad4de"; got != want {
		t.Fatalf("digest %s, ethers gives %s", got, want)
	}
	if got, want := sq.Signature.String(), "0x8b913d6945bf0f302f39cbd815cca35f5370a1a8e74c489bfa5c93f9636da36324afb34b28278559f75c78d271e9d75ac1bade57a421fbbd1fc3e9715dc024871c"; got != want {
		t.Fatalf("signature %s, ethers gives %s", got, want)
	}

	tampered := sq
	tampered.Query.Time++
	if _, err := tampered.Verify(); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered query: %v", err)
	}
	// A query's digest is never a transaction's, even with the same fields.
	tx := Transaction{Contract: q.Contract, Kind: KindCall, Agent: agent, Nonce: q.Nonce, Body: q.Body}
	if h, _ := tx.Hash(); h == digest {
		t.Fatal("query and transaction digests collide")
	}
}
