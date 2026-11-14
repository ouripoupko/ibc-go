package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/compiler"
	"github.com/ouripoupko/ibc-go/identity"
)

// lyingSender sends like HTTP, except that a node's answer to get_ledger is
// cut to its first record: a member that gives a joiner a fake history.
type lyingSender struct {
	inner common.HTTPSender
	liar  string // the URL of the lying node
}

func (s *lyingSender) Send(ctx context.Context, req common.Request) ([]byte, error) {
	body, err := s.inner.Send(ctx, req)
	raw, _ := json.Marshal(req.JSON)
	if err != nil || !strings.HasPrefix(req.URL, s.liar) || !strings.Contains(string(raw), "get_ledger") {
		return body, err
	}
	var reply struct {
		Records []json.RawMessage `json:"records"`
	}
	if json.Unmarshal(body, &reply) != nil || len(reply.Records) == 0 {
		return body, nil
	}
	return json.Marshal(map[string]any{"records": reply.Records[:1]})
}

// joinWith asks member to let u join contract, with the source hash given and,
// if publish, u's choice to make its copy public; it returns the join's id.
func (u *user) joinWith(t *testing.T, n, memberNode *testNode, member *user, contract string, sourceHash string, publish bool) string {
	t.Helper()
	stx := u.sign(t, identity.KindJoin, contract, map[string]any{"node": n.URL, "nodeKey": n.NodeKey(), "sourceHash": sourceHash})
	body := map[string]any{"tx": stx.Tx, "signature": stx.Signature,
		"partner": map[string]any{"address": memberNode.URL, "agent": member.id}}
	if publish {
		sp, err := identity.SignPublish(identity.Publish{Agent: u.addr, Contract: contract, Public: true,
			Time: uint64(time.Now().UnixMilli()), Nonce: identity.NewNonce()}, u.root)
		if err != nil {
			t.Fatal(err)
		}
		body["publish"] = sp
	}
	id, ok := call(t, n.URL, "PUT", u.id, "join_contract", "", "", body).(string)
	if !ok {
		t.Fatalf("join_contract replied %v", id)
	}
	return id
}

// A join names the source its agent read, and the joiner's node keeps the
// contract only if a member's history checks against the members' state and
// the source compiles to the deployed code.
func TestJoinChecks(t *testing.T) {
	a := startNode(t, Config{})
	alice, carol, bob, eve, dave := newUser(t), newUser(t), newUser(t), newUser(t), newUser(t)
	c := startNode(t, Config{})
	// Bob's node hears a fake history from alice's node.
	b := startNode(t, Config{Sender: &lyingSender{liar: a.URL}})
	e := startNode(t, Config{CheckSource: checkSource(compiler.Unchecked, "no solc 0.8.38 here")})
	d := startNode(t, Config{CheckSource: checkSource(compiler.Mismatch, "the source is not the deployed code")})
	ea, ec, eb, ee, ed := openStream(t, a.URL, alice, ""), openStream(t, c.URL, carol, ""), openStream(t, b.URL, bob, ""),
		openStream(t, e.URL, eve, ""), openStream(t, d.URL, dave, "")
	alice.register(t, a, ea)
	carol.register(t, c, ec)
	bob.register(t, b, eb)
	eve.register(t, e, ee)
	dave.register(t, d, ed)
	cid := alice.deploy(t, a, ea, "Registry", "", map[string]any{"initialName": "checked", "start": 0})
	alice.write(t, a, ea, cid, "setValue", map[string]any{"value": 5})
	source := identity.SourceHash(readSource(t, "Registry")).Hex()

	// The members refuse a join for another source.
	id := carol.joinWith(t, c, a, alice, cid, identity.SourceHash("another source").Hex(), false)
	if ev := ec.waitFor(t, id); ev["reply"] != false {
		t.Fatalf("a join for another source = %v", ev)
	}
	if got := carol.get(t, c, "get_contract", cid); len(got.(map[string]any)) != 0 {
		t.Fatalf("carol has the contract: %v", got)
	}
	id = carol.joinWith(t, c, a, alice, cid, source, false)
	if ev := ec.waitFor(t, id); ev["reply"] != true || ev["source_check"] != compiler.Verified {
		t.Fatalf("carol's join = %v", ev)
	}

	// Alice's node gives bob a fake history: bob's node takes carol's, which
	// the members' state confirms. Bob asked to make his copy public.
	id = bob.joinWith(t, b, a, alice, cid, source, true)
	if ev := eb.waitFor(t, id); ev["reply"] != true || ev["source_check"] != compiler.Verified {
		t.Fatalf("bob's join = %v", ev)
	}
	// Bob's node applies the history after the join.
	deadline := time.Now().Add(10 * time.Second)
	for bob.read(t, b, cid, "getValue", map[string]any{"who": alice.id}) != json.Number("5") {
		if time.Now().After(deadline) {
			t.Fatal("bob's node did not apply the history")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := publicRead(t, b, bob.id, cid, "counter", nil); got != json.Number("1") {
		t.Fatalf("bob's public copy: %v", got)
	}

	// Eve's node cannot compile the source: it joins, and says so.
	id = eve.joinWith(t, e, a, alice, cid, source, false)
	if ev := ee.waitFor(t, id); ev["reply"] != true || ev["source_check"] != compiler.Unchecked || ev["reason"] != "no solc 0.8.38 here" {
		t.Fatalf("eve's join = %v", ev)
	}
	if s := eve.get(t, e, "get_contract", cid).(map[string]any); s["source_check"] != compiler.Unchecked || s["source_check_reason"] == nil {
		t.Fatalf("eve's contract = %v", s)
	}

	// The source dave's node compiles is not the deployed code: it does not join.
	id = dave.joinWith(t, d, a, alice, cid, source, false)
	if ev := ed.waitFor(t, id); ev["reply"] != false || ev["source_check"] != compiler.Mismatch {
		t.Fatalf("dave's join = %v", ev)
	}
	if got := dave.get(t, d, "get_contract", cid); len(got.(map[string]any)) != 0 {
		t.Fatalf("dave has the contract: %v", got)
	}
}
