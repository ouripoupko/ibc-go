package node

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ouripoupko/ibc-go/identity"
)

// publicRead is an unsigned public_read of agent's copy of a contract.
func publicRead(t *testing.T, n *testNode, agent, contract, method string, values map[string]any) any {
	t.Helper()
	q := url.Values{"action": {"public_read"}}
	if values != nil {
		raw, _ := json.Marshal(values)
		q.Set("values", string(raw))
	}
	resp, err := http.Get(n.URL + "/ibc/app/" + agent + "/" + contract + "/" + method + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("public_read: missing CORS header")
	}
	raw, _ := io.ReadAll(resp.Body)
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("public_read: %v (%s)", err, raw)
	}
	return out
}

// setPublic sends u's signed choice for its copy of contract, signed by signer at time at.
func (u *user) setPublic(t *testing.T, n *testNode, signer identity.KeySigner, contract string, public bool, at time.Time) any {
	t.Helper()
	sp, err := identity.SignPublish(identity.Publish{Agent: u.addr, Contract: contract, Public: public,
		Time: uint64(at.UnixMilli()), Nonce: identity.NewNonce()}, signer)
	if err != nil {
		t.Fatal(err)
	}
	return call(t, n.URL, "PUT", u.id, "set_public", contract, "", sp)
}

func refused(got any, want string) bool {
	m, ok := got.(map[string]any)
	return ok && strings.Contains(m["message"].(string), want)
}

// Anyone may read a contract its agent made public, without a signature;
// only the agent's root decides, and only its own copy.
func TestPublicRead(t *testing.T) {
	n := startNode(t, Config{})
	alice, bob := newUser(t), newUser(t)
	events := openStream(t, n.URL, alice, "", bob, "")
	alice.register(t, n, events)
	bob.register(t, n, events)
	cid := alice.deploy(t, n, events, "Registry", "", map[string]any{"initialName": "open", "start": 0})
	alice.write(t, n, events, cid, "setValue", map[string]any{"value": 7})

	// Private by default; an unknown contract gets the same answer.
	if got := publicRead(t, n, alice.id, cid, "counter", nil); !refused(got, "no public contract") {
		t.Fatalf("read of a private contract = %v", got)
	}
	if got := publicRead(t, n, alice.id, "0x1234", "counter", nil); !refused(got, "no public contract") {
		t.Fatalf("read of an unknown contract = %v", got)
	}

	// Only alice's root decides for alice's copy.
	// bob has no copy of alice's contract to make public.
	if got := bob.setPublic(t, n, bob.root, cid, true, time.Now()); !refused(got, "has no contract") {
		t.Fatalf("bob's publish = %v", got)
	}
	forged, _ := identity.SignPublish(identity.Publish{Agent: alice.addr, Contract: cid, Public: true,
		Time: uint64(time.Now().UnixMilli()), Nonce: identity.NewNonce()}, bob.root)
	if got := call(t, n.URL, "PUT", alice.id, "set_public", cid, "", forged); !refused(got, "signature") {
		t.Fatalf("publish signed by bob for alice = %v", got)
	}
	if got := alice.setPublic(t, n, alice.root, cid, true, time.Now().Add(time.Hour)); !refused(got, "future") {
		t.Fatalf("publish from the future = %v", got)
	}

	start := time.Now()
	if got := alice.setPublic(t, n, alice.root, cid, true, start); got != true {
		t.Fatalf("set_public = %v", got)
	}
	if c := alice.get(t, n, "get_contract", cid).(map[string]any); c["public"] != true {
		t.Fatalf("summary public = %v", c["public"])
	}
	if got := publicRead(t, n, alice.id, cid, "getValue", map[string]any{"who": alice.id}); got != json.Number("7") {
		t.Fatalf("public getValue = %v", got)
	}
	if got := publicRead(t, n, alice.id, cid, "counter", nil); got != json.Number("1") {
		t.Fatalf("public counter = %v", got)
	}
	// Only reads: a function that changes state is refused.
	if got, _ := publicRead(t, n, alice.id, cid, "setValue", map[string]any{"value": 1}).(string); !strings.Contains(got, "changes state") {
		t.Fatalf("public setValue = %v", got)
	}
	// bob's agent has no copy: nothing public there.
	if got := publicRead(t, n, bob.id, cid, "counter", nil); !refused(got, "no public contract") {
		t.Fatalf("read of bob's copy = %v", got)
	}

	// Choices are ordered by their time: an older one, or the same again, is refused.
	if got := alice.setPublic(t, n, alice.root, cid, false, start.Add(-time.Minute)); !refused(got, "older") {
		t.Fatalf("an older publish = %v", got)
	}
	if got := alice.setPublic(t, n, alice.root, cid, false, start.Add(time.Second)); got != false {
		t.Fatalf("set_public false = %v", got)
	}
	if got := publicRead(t, n, alice.id, cid, "counter", nil); !refused(got, "no public contract") {
		t.Fatalf("read after making it private = %v", got)
	}
}
