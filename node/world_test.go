package node

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ouripoupko/ibc-go/identity"
)

// An agreement is a shared EVM world: World, its root contract, creates a
// Tally, which members call at its address; the node encodes and decodes for
// them, with the ABIs the deploy carries; and World decides who joins.
func TestWorld(t *testing.T) {
	a, b, c := startNode(t, Config{}), startNode(t, Config{}), startNode(t, Config{})
	alice, bob, carol := newUser(t), newUser(t), newUser(t)
	ea, eb, ec := openStream(t, a.URL, alice, ""), openStream(t, b.URL, bob, ""), openStream(t, c.URL, carol, "")
	alice.register(t, a, ea)
	bob.register(t, b, eb)
	carol.register(t, c, ec)
	cid := alice.deploy(t, a, ea, "World", "", nil)

	// The deploy carries the ABIs of both contracts, and World's membership function.
	summary := alice.get(t, a, "get_contract", cid).(map[string]any)
	if abis, _ := summary["abis"].(map[string]any); len(abis) != 2 || fmt.Sprint(summary["membership"]) != "[approve_partner]" {
		t.Fatalf("summary: abis %v, membership %v", summary["abis"], summary["membership"])
	}

	// The Tally's address, read from World, by name.
	tally, _ := alice.read(t, a, cid, "tally", nil).(string)
	if !strings.HasPrefix(tally, "0x") || len(tally) != 42 {
		t.Fatalf("tally = %v", tally)
	}
	// A transaction to the Tally: the event's reply is decoded.
	add := func(n int) map[string]any {
		data := calldata(t, "World", "add", map[string]any{"n": n})
		stx := alice.sign(t, identity.KindCall, cid, map[string]any{"to": tally, "data": data})
		id := call(t, a.URL, "POST", alice.id, "contract_write", cid, "", stx)
		return ea.waitFor(t, fmt.Sprint(id))
	}
	if ev := add(5); fmt.Sprint(ev["reply"]) != "5" || ev["selector"] == nil {
		t.Fatalf("add(5) = %v", ev)
	}
	// A custom error, decoded from the source's ABIs.
	if ev := add(101); ev["error"] != "execution reverted: TooMuch(101)" {
		t.Fatalf("add(101) = %v", ev)
	}
	// A read at the Tally's address: msg.sender was alice.
	read := func(u *user, n *testNode, method string) any {
		return call(t, n.URL, "POST", u.id, "contract_read", cid, "",
			u.query(t, "contract_read", u.id, cid, map[string]any{"to": tally, "method": method, "values": map[string]any{}}))
	}
	if got := read(alice, a, "last"); got != alice.id {
		t.Fatalf("last = %v, want alice", got)
	}
	// Raw calldata reads too, and its result comes back decoded.
	if got := call(t, a.URL, "POST", alice.id, "contract_read", cid, "", alice.query(t, "contract_read", alice.id, cid,
		map[string]any{"to": tally, "data": calldata(t, "World", "total", nil)})); fmt.Sprint(got) != "5" {
		t.Fatalf("total, by calldata = %v", got)
	}

	// World decides who joins: carol, not invited, is refused; bob, invited, joins.
	if ev := ec.waitFor(t, carol.joinWith(t, c, a, alice, cid, identity.SourceHash(readSource(t, "World")).Hex(), false)); ev["reply"] != false {
		t.Fatalf("carol, not invited, joins: %v", ev)
	}
	invite := alice.sign(t, identity.KindCall, cid, map[string]any{"data": calldata(t, "World", "invite", map[string]any{"who": bob.id})})
	if ev := ea.waitFor(t, fmt.Sprint(call(t, a.URL, "POST", alice.id, "contract_write", cid, "", invite))); ev["error"] != nil {
		t.Fatalf("invite = %v", ev)
	}
	if ev := eb.waitFor(t, bob.joinWith(t, b, a, alice, cid, identity.SourceHash(readSource(t, "World")).Hex(), false)); ev["reply"] != true {
		t.Fatalf("bob, invited, joins: %v", ev)
	}
	// Bob's node, a partner now, reads the Tally at the same address, once
	// it has applied the history.
	deadline := time.Now().Add(10 * time.Second)
	for got := read(bob, b, "total"); fmt.Sprint(got) != "5"; got = read(bob, b, "total") {
		if time.Now().After(deadline) {
			t.Fatalf("bob reads total = %v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
