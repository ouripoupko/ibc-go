package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ouripoupko/ibc-go/identity"
)

// testNode is a node behind a real HTTP server.
type testNode struct {
	*Node
	URL string
}

func startNode(t *testing.T, cfg Config) *testNode {
	t.Helper()
	if cfg.DB == nil && cfg.DataDir == "" {
		cfg.DB = rawdb.NewMemoryDatabase()
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(n.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = n.Close() }) // runs first: ends the event streams
	return &testNode{Node: n, URL: srv.URL}
}

// call is the JS ibc() helper from blockchain-api.md.
func call(t *testing.T, base, verb, identity, action, contract, method string, body any) any {
	t.Helper()
	path := strings.Join(nonEmpty(identity, contract, method), "/")
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(verb, base+"/ibc/app/"+path+"?action="+action, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %s", verb, action, resp.Status)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("%s %s: missing CORS header", verb, action)
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("%s %s: %v", verb, action, err)
	}
	return out
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stream is an SSE client.
type stream struct {
	events chan map[string]any
	body   io.Closer
}

// openStream opens the event stream of each (user, contract) pair; an empty
// contract means all of the user's contracts.
func openStream(t *testing.T, base string, pairs ...any) *stream {
	t.Helper()
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		u, contract := pairs[i].(*user), pairs[i+1].(string)
		q.Add("auth", u.auth(t, "stream", u.id, contract, nil))
	}
	resp, err := http.Get(base + "/stream?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content type %q", ct)
	}
	s := &stream{events: make(chan map[string]any, 100), body: resp.Body}
	connected := make(chan struct{})
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		first := true
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			if first {
				close(connected) // the initial keep-alive: we are subscribed
				first = false
			}
			if data == "" {
				continue
			}
			dec := json.NewDecoder(strings.NewReader(data))
			dec.UseNumber()
			var ev map[string]any
			if dec.Decode(&ev) == nil {
				s.events <- ev
			}
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not connect")
	}
	return s
}

// waitFor returns the event answering request.
func (s *stream) waitFor(t *testing.T, request any) map[string]any {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatalf("stream closed while waiting for %v", request)
			}
			if ev["request"] == request {
				return ev
			}
		case <-timeout:
			t.Fatalf("no event for request %v", request)
		}
	}
}

// waitAction returns the next event with the given action.
func (s *stream) waitAction(t *testing.T, action string) map[string]any {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatalf("stream closed while waiting for %s", action)
			}
			if ev["action"] == action {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", action)
		}
	}
}

// user is an agent's owner, with the key a phone would hold: a root passkey
// on the node's domain, which signs everything the user approves. (The
// browser test uses real WebAuthn; this one is simulated.)
type user struct {
	root       identity.WebAuthnSigner
	credential []byte // the root passkey's credential id
	addr       ethcommon.Address
	id         string // the agent id: the root key's address
}

func newUser(t *testing.T) *user {
	t.Helper()
	rootKey, err := identity.NewP256Key()
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := identity.AddressOf(identity.PublicKey(rootKey))
	return &user{root: identity.WebAuthnSigner{Key: rootKey, RPID: "localhost"},
		credential: []byte(identity.NewNonce()), addr: addr, id: addr.Hex()}
}

// connectApp records u's consent on n for an app's key: for contracts of
// codeHash, reads without asking if read, and the functions n may sign
// silently, with the grant for n signed by u's root.
func (u *user) connectApp(t *testing.T, n *testNode, appKey *ecdsa.PrivateKey, codeHash ethcommon.Hash, read bool, functions ...string) any {
	t.Helper()
	consent, err := identity.SignConsent(identity.Consent{Agent: u.addr, App: "https://app.example",
		AppKey: identity.PublicKey(appKey), CodeHash: codeHash, Read: read, Functions: functions, Nonce: identity.NewNonce()}, u.root)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"consent": consent}
	if len(functions) > 0 {
		grant, err := identity.CertifyGrant(identity.Grant{Agent: u.addr, NodeKey: ethcommon.HexToAddress(n.NodeKey()),
			CodeHash: codeHash, Functions: functions, Nonce: identity.NewNonce()}, u.root)
		if err != nil {
			t.Fatal(err)
		}
		body["grant"] = grant
	}
	return call(t, n.URL, "PUT", u.id, "register_app", "", "", body)
}

// register registers the user's agent on n, authorizing n's key.
func (u *user) register(t *testing.T, n *testNode, events *stream) {
	t.Helper()
	info := call(t, n.URL, "GET", "", "node_info", "", "", nil).(map[string]any)
	sr, err := identity.SignRegistration(identity.Registration{Agent: u.addr, Node: n.URL,
		NodeKey: ethcommon.HexToAddress(info["nodeKey"].(string)), Credential: u.credential, Nonce: identity.NewNonce()}, u.root)
	if err != nil {
		t.Fatal(err)
	}
	ack := call(t, n.URL, "PUT", u.id, "register_agent", "", "", sr)
	if ev := events.waitFor(t, ack); ev["reply"] != true {
		t.Fatalf("register event = %v", ev)
	}
}

// query signs a query for action on target's copy of contract, with u's
// write key; body, if not nil, is marshalled to the JSON body string.
func (u *user) query(t *testing.T, action, target, contract string, body any) identity.SignedQuery {
	t.Helper()
	return u.queryWith(t, u.root, action, target, contract, body)
}

// queryWith signs a query with the given key of u.
func (u *user) queryWith(t *testing.T, key identity.KeySigner, action, target, contract string, body any) identity.SignedQuery {
	t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	sq, err := identity.SignQuery(identity.Query{Agent: u.addr, Action: action, Target: target,
		Contract: contract, Body: raw, Time: uint64(time.Now().Unix()), Nonce: identity.NewNonce()}, key)
	if err != nil {
		t.Fatal(err)
	}
	return sq
}

// auth is a signed query as the JSON of an auth URL parameter.
func (u *user) auth(t *testing.T, action, target, contract string, body any) string {
	t.Helper()
	raw, _ := json.Marshal(u.query(t, action, target, contract, body))
	return string(raw)
}

// get makes a signed GET (get_contract, get_contracts) for u's own agent.
func (u *user) get(t *testing.T, n *testNode, action, contract string) any {
	t.Helper()
	return getAuth(t, n.URL, u.id, action, contract, u.auth(t, action, u.id, contract, nil))
}

func getAuth(t *testing.T, base, agent, action, contract, auth string) any {
	t.Helper()
	path := strings.Join(nonEmpty(agent, contract), "/")
	resp, err := http.Get(base + "/ibc/app/" + path + "?" + url.Values{"action": {action}, "auth": {auth}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("GET %s: %v", action, err)
	}
	return out
}

// sign signs a transaction; body is marshalled to the JSON body string.
func (u *user) sign(t *testing.T, kind, contract string, body any) identity.SignedTx {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	stx, err := identity.SignTx(identity.Transaction{Contract: contract, Kind: kind, Agent: u.addr,
		Nonce: identity.NewNonce(), Body: string(raw)}, u.root)
	if err != nil {
		t.Fatal(err)
	}
	return stx
}

func readTestdata(t *testing.T, name string) (code, abi string) {
	t.Helper()
	bin, err := os.ReadFile("../testdata/" + name + ".bin")
	if err != nil {
		t.Fatal(err)
	}
	abiJSON, err := os.ReadFile("../testdata/" + name + ".abi")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(bin)), string(abiJSON)
}

// deploy deploys a test contract from u on n and waits until it is live.
func (u *user) deploy(t *testing.T, n *testNode, events *stream, name, protocol string, ctor map[string]any) string {
	t.Helper()
	code, abi := readTestdata(t, name)
	stx := u.sign(t, identity.KindDeploy, "", map[string]any{
		"name": "my-" + name, "contract": name + ".sol", "protocol": protocol, "default_app": "https://app.example",
		"profile": "", "code": code, "abi": json.RawMessage(abi), "constructor": ctor,
		"node": n.URL, "nodeKey": n.NodeKey(),
	})
	id := call(t, n.URL, "PUT", u.id, "deploy_contract", "", "", stx)
	contractID, ok := id.(string)
	if !ok {
		t.Fatalf("deploy_contract replied %v", id)
	}
	if ev := events.waitFor(t, contractID); ev["reply"] != true {
		t.Fatalf("deploy event = %v", ev)
	}
	return contractID
}

// write sends a call and returns its outcome event.
func (u *user) write(t *testing.T, n *testNode, events *stream, contract, method string, values map[string]any) map[string]any {
	t.Helper()
	stx := u.sign(t, identity.KindCall, contract, map[string]any{"method": method, "values": values})
	id := call(t, n.URL, "POST", u.id, "contract_write", contract, method, stx)
	if _, ok := id.(string); !ok {
		t.Fatalf("contract_write replied %v", id)
	}
	return events.waitFor(t, id)
}

// read reads owner's copy of a contract on n, signed by u.
func (u *user) readAs(t *testing.T, n *testNode, owner, contract, method string, values map[string]any) any {
	t.Helper()
	if values == nil {
		values = map[string]any{}
	}
	return call(t, n.URL, "POST", owner, "contract_read", contract, method,
		u.query(t, "contract_read", owner, contract, map[string]any{"method": method, "values": values}))
}

func (u *user) read(t *testing.T, n *testNode, contract, method string, values map[string]any) any {
	t.Helper()
	return u.readAs(t, n, u.id, contract, method, values)
}

// join asks member (on memberNode) to let u join, from u's node n.
func (u *user) join(t *testing.T, n, memberNode *testNode, member *user, contract string) any {
	t.Helper()
	stx := u.sign(t, identity.KindJoin, contract, map[string]any{"node": n.URL, "nodeKey": n.NodeKey(), "profile": ""})
	body := map[string]any{"tx": stx.Tx, "signature": stx.Signature,
		"partner": map[string]any{"address": memberNode.URL, "agent": member.id}}
	return call(t, n.URL, "PUT", u.id, "join_contract", "", "", body)
}

// The client API on one node: registration, deploy, writes, reads.
func TestClientAPI(t *testing.T) {
	a := startNode(t, Config{})
	alice := newUser(t)

	if got := call(t, a.URL, "GET", alice.id, "is_exist_agent", "", "", nil); got != false {
		t.Fatalf("is_exist_agent before register = %v", got)
	}
	events := openStream(t, a.URL, alice, "")
	alice.register(t, a, events)
	if got := call(t, a.URL, "GET", strings.ToLower(alice.id), "is_exist_agent", "", "", nil); got != true {
		t.Fatalf("is_exist_agent (lower-case address) after register = %v", got)
	}

	cid := alice.deploy(t, a, events, "Registry", "", map[string]any{"initialName": "registry", "start": 7})
	if ev := alice.write(t, a, events, cid, "setValue", map[string]any{"value": 42}); ev["reply"] != json.Number("8") || ev["state_root"] == "" {
		t.Fatalf("write event = %v", ev)
	}
	if got := alice.read(t, a, cid, "getValue", map[string]any{"who": alice.id}); got != json.Number("42") {
		t.Fatalf("contract_read getValue = %v", got)
	}
	// msg.sender is the agent's own address.
	info := alice.read(t, a, cid, "info", nil).([]any)
	if info[3] != alice.id {
		t.Fatalf("msg.sender = %v, want %s", info[3], alice.id)
	}
	if ev := alice.write(t, a, events, cid, "setThenFail", map[string]any{"value": 1}); ev["reply"] != "execution reverted: setThenFail: rejected" {
		t.Fatalf("revert event = %v", ev)
	}
	if got := alice.read(t, a, "missing", "counter", nil); fmt.Sprint(got) != "map[message:no such contract]" {
		t.Fatalf("contract_read missing contract = %v", got)
	}

	// A forged transaction and a transaction sent as someone else are refused.
	forged := alice.sign(t, identity.KindCall, cid, map[string]any{"method": "setValue", "values": map[string]any{"value": 1}})
	forged.Tx.Nonce = "changed"
	if got := call(t, a.URL, "POST", alice.id, "contract_write", cid, "setValue", forged); !strings.Contains(fmt.Sprint(got), "signature") {
		t.Fatalf("forged transaction: %v", got)
	}
	mallory := newUser(t)
	honest := alice.sign(t, identity.KindCall, cid, map[string]any{"method": "setValue", "values": map[string]any{"value": 1}})
	if got := call(t, a.URL, "POST", mallory.id, "contract_write", cid, "setValue", honest); !strings.Contains(fmt.Sprint(got), "sent as") {
		t.Fatalf("transaction sent as another agent: %v", got)
	}

	one := alice.get(t, a, "get_contract", cid).(map[string]any)
	if one["id"] != cid || one["name"] != "my-Registry" || one["pid"] != alice.id || one["evm_address"] == nil {
		t.Fatalf("get_contract = %v", one)
	}
	if list := alice.get(t, a, "get_contracts", "").([]any); len(list) != 1 {
		t.Fatalf("get_contracts = %v", list)
	}
	// An unregistered agent cannot deploy.
	code, abi := readTestdata(t, "Registry")
	deploy := mallory.sign(t, identity.KindDeploy, "", map[string]any{"code": code, "abi": json.RawMessage(abi),
		"constructor": map[string]any{"initialName": "x", "start": 0}, "node": a.URL, "nodeKey": a.NodeKey()})
	if got := call(t, a.URL, "PUT", mallory.id, "deploy_contract", "", "", deploy); !strings.Contains(fmt.Sprint(got), "unknown agent") {
		t.Fatalf("deploy by an unregistered agent: %v", got)
	}
	if got := call(t, a.URL, "PUT", alice.id, "no_such_action", "", "", nil); !strings.Contains(fmt.Sprint(got), "unknown action") {
		t.Fatalf("unknown action = %v", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	a := startNode(t, Config{})
	req, _ := http.NewRequest(http.MethodOptions, a.URL+"/ibc/app/alice?action=register_agent", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "PUT") {
		t.Fatalf("preflight: %s %v", resp.Status, resp.Header)
	}
}

// Bob, on another node, asks to join Alice's contract. Alice's node answers
// (not approved: approve_partner is a stub) and Bob's client hears it.
// Three agents on three nodes share a contract: two join, everyone writes,
// and every node reaches the same state.
func TestPartnersShareAContract(t *testing.T) {
	nodes := []*testNode{startNode(t, Config{}), startNode(t, Config{}), startNode(t, Config{})}
	users := []*user{newUser(t), newUser(t), newUser(t)}
	var events []*stream
	for i, u := range users {
		events = append(events, openStream(t, nodes[i].URL, u, ""))
		u.register(t, nodes[i], events[i])
	}
	alice, bob, carol := users[0], users[1], users[2]
	cid := alice.deploy(t, nodes[0], events[0], "Registry", "", map[string]any{"initialName": "shared", "start": 0})
	alice.write(t, nodes[0], events[0], cid, "setValue", map[string]any{"value": 1})

	// bob joins through alice; carol joins through bob.
	if id, ok := bob.join(t, nodes[1], nodes[0], alice, cid).(string); !ok || len(id) != 66 {
		t.Fatalf("bob's join_contract replied %v", id)
	}
	if ev := events[1].waitAction(t, "join_contract"); ev["reply"] != true {
		t.Fatalf("bob's join event = %v", ev)
	}
	// A newcomer's first write is ordered without anyone else writing.
	if ev := bob.write(t, nodes[1], events[1], cid, "setValue", map[string]any{"value": 2}); ev["reply"] != json.Number("2") {
		t.Fatalf("bob's first write = %v", ev)
	}
	if id, ok := carol.join(t, nodes[2], nodes[1], bob, cid).(string); !ok || len(id) != 66 {
		t.Fatalf("carol's join_contract replied %v", id)
	}
	if ev := events[2].waitAction(t, "join_contract"); ev["reply"] != true {
		t.Fatalf("carol's join event = %v", ev)
	}

	// Everyone writes, at the same time.
	done := make(chan map[string]any, 3)
	for i, u := range users {
		go func() { done <- u.write(t, nodes[i], events[i], cid, "setValue", map[string]any{"value": 10 + i}) }()
	}
	for range users {
		if ev := <-done; ev["action"] != "contract_write" {
			t.Fatalf("write event = %v", ev)
		}
	}
	// Every node agrees on every value and on the state root.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var roots []any
		agree := true
		for i, n := range nodes {
			c := users[i].get(t, n, "get_contract", cid).(map[string]any)
			roots = append(roots, c["state_root"])
			if users[i].read(t, n, cid, "counter", nil) != json.Number("5") {
				agree = false
			}
		}
		if agree && roots[0] == roots[1] && roots[1] == roots[2] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nodes did not converge: roots %v", roots)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i, n := range nodes {
		for j, u := range users {
			if got := users[i].read(t, n, cid, "getValue", map[string]any{"who": u.id}); got != json.Number(fmt.Sprint(10+j)) {
				t.Errorf("node %d: value of user %d = %v", i, j, got)
			}
		}
	}
}

// Reads, contract details and streams need a signed query; node messages
// need a node's signature, and only members' nodes are answered.
func TestSignatures(t *testing.T) {
	a := startNode(t, Config{})
	alice, bob := newUser(t), newUser(t)
	events := openStream(t, a.URL, alice, "")
	alice.register(t, a, events)
	cid := alice.deploy(t, a, events, "Registry", "", map[string]any{"initialName": "q", "start": 0})
	refused := func(what string, got any, want string) {
		t.Helper()
		if !strings.Contains(fmt.Sprint(got), want) {
			t.Fatalf("%s: got %v, want an error with %q", what, got, want)
		}
	}
	readBody := map[string]any{"method": "counter", "values": map[string]any{}}

	refused("unsigned read", call(t, a.URL, "POST", alice.id, "contract_read", cid, "counter",
		map[string]any{"values": map[string]any{}}), "signed query")
	// An agent reads only its own copy: bob cannot read alice's.
	refused("bob reads alice's copy", bob.readAs(t, a, alice.id, cid, "counter", nil), "query for agent")
	sq := alice.query(t, "contract_read", alice.id, cid, readBody)
	if got := call(t, a.URL, "POST", alice.id, "contract_read", cid, "", sq); got != json.Number("0") {
		t.Fatalf("signed read = %v", got)
	}
	refused("replayed read", call(t, a.URL, "POST", alice.id, "contract_read", cid, "", sq), "already used")
	old := alice.query(t, "contract_read", alice.id, cid, readBody)
	old.Query.Time -= 120
	if old, _ = identity.SignQuery(old.Query, alice.root); true {
		refused("expired read", call(t, a.URL, "POST", alice.id, "contract_read", cid, "", old), "expired")
	}
	refused("read signed for another action", call(t, a.URL, "POST", alice.id, "contract_read", cid, "",
		alice.query(t, "get_contract", alice.id, cid, readBody)), "sent to contract_read")
	forged := alice.query(t, "contract_read", alice.id, cid, readBody)
	forged.Query.Body = `{"method":"info","values":{}}`
	refused("forged read", call(t, a.URL, "POST", alice.id, "contract_read", cid, "", forged), "signature")

	// Only the agent itself sees its contracts and its stream.
	refused("unsigned get_contracts", getAuth(t, a.URL, alice.id, "get_contracts", "", ""), "signed query")
	refused("bob lists alice's contracts", getAuth(t, a.URL, alice.id, "get_contracts", "",
		bob.auth(t, "get_contracts", alice.id, "", nil)), "query for agent")
	for _, auth := range []string{"", bob.auth(t, "stream", alice.id, "", nil)} {
		resp, err := http.Get(a.URL + "/stream?" + url.Values{"auth": {auth}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("stream with auth %q: status %s", auth, resp.Status)
		}
	}

	// Node to node: an unsigned message is refused, and a signed one from a
	// node that serves no member is not answered.
	refused("unsigned a2a_consent", call(t, a.URL, "PUT", alice.id, "a2a_consent", cid, "",
		map[string]any{"msg": map[string]any{"type": "get_ledger"}}), "signature")
	stranger, _ := crypto.GenerateKey()
	msg := []byte(`{"type":"get_ledger"}`)
	now := uint64(time.Now().Unix())
	hash := crypto.Keccak256Hash([]byte(fmt.Sprintf("ibc a2a v1\n%s\n%s\n%d\n", alice.id, cid, now)), msg)
	sig, _ := identity.SignHash(hash, stranger)
	env := map[string]any{"msg": json.RawMessage(msg), "node": identity.Address(stranger).Hex(), "time": now,
		"signature": "0x" + fmt.Sprintf("%x", sig)}
	refused("get_ledger from a stranger's node", call(t, a.URL, "PUT", alice.id, "a2a_consent", cid, "", env),
		"serves no member")
}

// Apps the user connects: the node reads for them, and signs the calls the
// user allowed, under the user's grant, which partners check.
func TestApps(t *testing.T) {
	a, b := startNode(t, Config{}), startNode(t, Config{})
	alice, bob := newUser(t), newUser(t)
	ea, eb := openStream(t, a.URL, alice, ""), openStream(t, b.URL, bob, "")
	alice.register(t, a, ea)
	bob.register(t, b, eb)
	cid := alice.deploy(t, a, ea, "Registry", "", map[string]any{"initialName": "apps", "start": 0})
	bob.join(t, b, a, alice, cid)
	if ev := eb.waitAction(t, "join_contract"); ev["reply"] != true {
		t.Fatalf("bob's join = %v", ev)
	}
	refused := func(what string, got any, want string) {
		t.Helper()
		if !strings.Contains(fmt.Sprint(got), want) {
			t.Fatalf("%s: got %v, want an error with %q", what, got, want)
		}
	}
	code, _ := readTestdata(t, "Registry")
	codeHash, _ := identity.CodeHash(code)
	app, _ := identity.NewP256Key()
	appQuery := func(action string, body any) identity.SignedQuery {
		return alice.queryWith(t, identity.P256Signer{Key: app}, action, alice.id, cid, body)
	}
	write := func(value int) any {
		return call(t, a.URL, "POST", alice.id, "contract_write", cid, "setValue",
			appQuery("contract_write", map[string]any{"method": "setValue", "values": map[string]any{"value": value}}))
	}

	refused("an app alice did not connect", write(7), "nor an app it connected")
	if got := alice.connectApp(t, a, app, codeHash, true, "setValue"); got != true {
		t.Fatalf("connect = %v", got)
	}
	// The node signs the app's call silently, under alice's grant; bob's node,
	// a partner, checks the grant and applies it.
	id, ok := write(7).(string)
	if !ok {
		t.Fatalf("silent write replied %v", id)
	}
	if ev := ea.waitFor(t, id); ev["reply"] != json.Number("1") {
		t.Fatalf("silent write = %v", ev)
	}
	deadline := time.Now().Add(15 * time.Second)
	for bob.read(t, b, cid, "getValue", map[string]any{"who": alice.id}) != json.Number("7") {
		if time.Now().After(deadline) {
			t.Fatal("bob's node did not apply the node-signed call")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := call(t, a.URL, "POST", alice.id, "contract_read", cid, "", appQuery("contract_read",
		map[string]any{"method": "getValue", "values": map[string]any{"who": alice.id}})); got != json.Number("7") {
		t.Fatalf("app's silent read = %v", got)
	}
	refused("a function alice did not allow", call(t, a.URL, "POST", alice.id, "contract_write", cid, "setThenFail",
		appQuery("contract_write", map[string]any{"method": "setThenFail", "values": map[string]any{"value": 1}})), "needs your approval")

	// A node signing under a grant for other code: partners refuse it in
	// execution (the call is recorded, and changes nothing).
	otherCode := crypto.Keccak256Hash([]byte("other code"))
	wrong, _ := identity.CertifyGrant(identity.Grant{Agent: alice.addr, NodeKey: identity.Address(a.nodeKey),
		CodeHash: otherCode, Functions: []string{"setValue"}, Nonce: "n"}, alice.root)
	stx, _ := identity.SignTx(identity.Transaction{Contract: cid, Kind: identity.KindCall, Agent: alice.addr, Nonce: "w",
		Body: `{"method":"setValue","values":{"value":99}}`}, identity.Secp256k1Signer{Key: a.nodeKey, Cert: wrong})
	wid := call(t, a.URL, "POST", alice.id, "contract_write", cid, "setValue", stx)
	if ev := ea.waitFor(t, wid); ev["reply"] != "the node's grant does not apply: it is for other code" {
		t.Fatalf("call under a grant for other code = %v", ev)
	}

	// The same app in another browser: a new key, which Alice connects with
	// one signature; the app's permissions apply to it, nothing to choose again.
	app2, _ := identity.NewP256Key()
	write2 := func(value int) any {
		return call(t, a.URL, "POST", alice.id, "contract_write", cid, "setValue",
			alice.queryWith(t, identity.P256Signer{Key: app2}, "contract_write", alice.id, cid,
				map[string]any{"method": "setValue", "values": map[string]any{"value": value}}))
	}
	refused("a browser alice did not connect", write2(9), "nor an app it connected")
	device, _ := identity.SignDevice(identity.Device{Agent: alice.addr, App: "https://app.example",
		AppKey: identity.PublicKey(app2), Nonce: identity.NewNonce()}, alice.root)
	if got := call(t, a.URL, "PUT", alice.id, "register_app", "", "", map[string]any{"device": device}); got != true {
		t.Fatalf("connect a device = %v", got)
	}
	id, ok = write2(9).(string)
	if !ok {
		t.Fatalf("silent write from the second browser = %v", id)
	}
	if ev := ea.waitFor(t, id); ev["reply"] != json.Number("2") {
		t.Fatalf("silent write from the second browser = %v", ev)
	}
	otherSite, _ := identity.SignDevice(identity.Device{Agent: alice.addr, App: "https://unknown.example",
		AppKey: identity.PublicKey(app2), Nonce: "n"}, alice.root)
	refused("one browser key for two sites", call(t, a.URL, "PUT", alice.id, "register_app", "", "",
		map[string]any{"device": otherSite}), "belongs to")

	// Connecting a new app: it learns who alice is, and may do nothing without asking.
	app3, _ := identity.NewP256Key()
	connect, _ := identity.SignDevice(identity.Device{Agent: alice.addr, App: "https://new.example",
		AppKey: identity.PublicKey(app3), Nonce: identity.NewNonce()}, alice.root)
	if got := call(t, a.URL, "PUT", alice.id, "register_app", "", "", map[string]any{"device": connect}); got != true {
		t.Fatalf("connect a new app = %v", got)
	}
	refused("a connected app's read", call(t, a.URL, "POST", alice.id, "contract_read", cid, "getValue",
		alice.queryWith(t, identity.P256Signer{Key: app3}, "contract_read", alice.id, cid,
			map[string]any{"method": "getValue", "values": map[string]any{"who": alice.id}})), "needs your approval")
	listAuth, _ := json.Marshal(alice.queryWith(t, identity.P256Signer{Key: app3}, "get_contracts", alice.id, "", nil))
	if got := getAuth(t, a.URL, alice.id, "get_contracts", "", string(listAuth)); fmt.Sprint(got) != "[]" {
		t.Fatalf("a connected app's contracts = %v", got)
	}
	if got := call(t, a.URL, "PUT", alice.id, "revoke_app", "", "", alice.queryWith(t, alice.root, "revoke_app",
		alice.id, "", map[string]any{"app": "https://new.example"})); got != true {
		t.Fatalf("revoke the new app = %v", got)
	}

	// Alice sees her apps, and disconnects one, with all its browsers, with her root key.
	apps := getAuth(t, a.URL, alice.id, "get_apps", "", alice.auth(t, "get_apps", alice.id, "", nil)).([]any)
	if len(apps) != 1 || apps[0].(map[string]any)["app"] != "https://app.example" || len(apps[0].(map[string]any)["keys"].([]any)) != 2 {
		t.Fatalf("get_apps = %v", apps)
	}
	revoke := alice.queryWith(t, alice.root, "revoke_app", alice.id, "", map[string]any{"app": "https://app.example"})
	if got := call(t, a.URL, "PUT", alice.id, "revoke_app", "", "", revoke); got != true {
		t.Fatalf("revoke = %v", got)
	}
	refused("the second browser of a revoked app", write2(10), "nor an app it connected")
	refused("a revoked app", write(8), "nor an app it connected")

	// A grant must name this node; the root key alone registers.
	other, _ := identity.NewP256Key()
	consent, _ := identity.SignConsent(identity.Consent{Agent: alice.addr, App: "https://x", AppKey: identity.PublicKey(other),
		CodeHash: codeHash, Functions: []string{"setValue"}, Nonce: "n"}, alice.root)
	grant, _ := identity.CertifyGrant(identity.Grant{Agent: alice.addr, NodeKey: ethcommon.HexToAddress(b.NodeKey()),
		CodeHash: codeHash, Functions: []string{"setValue"}, Nonce: "n"}, alice.root)
	refused("a grant for another node", call(t, a.URL, "PUT", alice.id, "register_app", "", "",
		map[string]any{"consent": consent, "grant": grant}), "names another node")
	byApp, _ := identity.SignConsent(consent.Consent, identity.P256Signer{Key: other})
	refused("a consent the app signed itself", call(t, a.URL, "PUT", alice.id, "register_app", "", "",
		map[string]any{"consent": byApp}), "signature")

	// agent_info finds the agent from its root passkey, for a new device.
	resp, err := http.Get(a.URL + "/ibc/app?action=agent_info&credential=" + hexutil.Encode(alice.credential))
	if err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&found)
	resp.Body.Close()
	if found["agent"] != alice.id || found["rootScheme"] != "webauthn" {
		t.Fatalf("agent_info by credential = %v", found)
	}
}

// A contract's approve_partner refuses an agent it did not invite.
func TestJoinRefusedByContractPolicy(t *testing.T) {
	a, b := startNode(t, Config{}), startNode(t, Config{})
	owner, stranger := newUser(t), newUser(t)
	ea, eb := openStream(t, a.URL, owner, ""), openStream(t, b.URL, stranger, "")
	owner.register(t, a, ea)
	stranger.register(t, b, eb)
	cid := owner.deploy(t, a, ea, "Club", "", nil)
	stranger.join(t, b, a, owner, cid)
	if ev := eb.waitAction(t, "join_contract"); ev["reply"] != false {
		t.Fatalf("join event = %v", ev)
	}
	if got := stranger.get(t, b, "get_contract", cid); fmt.Sprint(got) != "map[]" {
		t.Fatalf("the refused agent has the contract: %v", got)
	}
}

func TestImmediateProtocol(t *testing.T) {
	a := startNode(t, Config{})
	alice := newUser(t)
	events := openStream(t, a.URL, alice, "")
	alice.register(t, a, events)
	cid := alice.deploy(t, a, events, "Registry", "immediate", map[string]any{"initialName": "solo", "start": 0})
	if ev := alice.write(t, a, events, cid, "setValue", map[string]any{"value": 5}); ev["reply"] != json.Number("1") {
		t.Fatalf("write = %v", ev)
	}
}

func TestStreamScopes(t *testing.T) {
	a := startNode(t, Config{})
	alice := newUser(t)
	setup := openStream(t, a.URL, alice, "")
	alice.register(t, a, setup)
	c1 := alice.deploy(t, a, setup, "Registry", "", map[string]any{"initialName": "one", "start": 0})
	c2 := alice.deploy(t, a, setup, "Registry", "", map[string]any{"initialName": "two", "start": 0})

	all := openStream(t, a.URL, alice, "")
	only1 := openStream(t, a.URL, alice, c1)
	other := openStream(t, a.URL, newUser(t), "")
	w2 := alice.write(t, a, setup, c2, "setValue", map[string]any{"value": 1})["request"]
	w1 := alice.write(t, a, setup, c1, "setValue", map[string]any{"value": 1})["request"]
	seen := map[any]bool{}
	for len(seen) < 2 {
		seen[all.waitAction(t, "contract_write")["request"]] = true
	}
	if !seen[w1] || !seen[w2] {
		t.Fatalf("all-contracts stream saw %v", seen)
	}
	if ev := only1.waitAction(t, "contract_write"); ev["request"] != w1 {
		t.Fatalf("contract-scoped stream got %v, want only %v", ev, w1)
	}
	select {
	case ev := <-other.events:
		t.Fatalf("another agent's stream got %v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDataSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a := startNode(t, Config{DataDir: dir})
	alice := newUser(t)
	events := openStream(t, a.URL, alice, "")
	alice.register(t, a, events)
	cid := alice.deploy(t, a, events, "Registry", "", map[string]any{"initialName": "kept", "start": 0})
	alice.write(t, a, events, cid, "setValue", map[string]any{"value": 5})
	nodeKey := a.NodeKey()
	// node_info reports the size of the data folder.
	info := call(t, a.URL, "GET", "", "node_info", "", "", nil).(map[string]any)
	if size, err := info["storage"].(json.Number).Int64(); err != nil || size <= 0 {
		t.Fatalf("node_info storage = %v", info["storage"])
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b := startNode(t, Config{DataDir: dir})
	if b.NodeKey() != nodeKey {
		t.Fatalf("node key changed across a restart: %s, %s", nodeKey, b.NodeKey())
	}
	if got := alice.read(t, b, cid, "counter", nil); got != json.Number("1") {
		t.Fatalf("counter after restart = %v", got)
	}
	// Consensus resumes: a new write is ordered and applied.
	events = openStream(t, b.URL, alice, "")
	if ev := alice.write(t, b, events, cid, "setValue", map[string]any{"value": 6}); ev["reply"] != json.Number("2") {
		t.Fatalf("write after restart = %v", ev)
	}
}

func TestServeShutsDownWithOpenStreams(t *testing.T) {
	n, err := New(Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- n.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()
	s := openStream(t, base, newUser(t), "")

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return with an open event stream")
	}
	for range s.events { // the stream ends
	}
}

// The node serves its web interface next to the API.
func TestWebInterface(t *testing.T) {
	a := startNode(t, Config{})
	for path, want := range map[string]string{
		"/":              "text/html",
		"/contract.html": "text/html",
		"/ibc.mjs":       "text/javascript",
		"/app.mjs":       "text/javascript",
		"/style.css":     "text/css",
	} {
		resp, err := http.Get(a.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), want) {
			t.Errorf("GET %s: %s, %q", path, resp.Status, resp.Header.Get("Content-Type"))
		}
	}
	resp, err := http.Post(a.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %s", resp.Status)
	}
	// The API is still reached under /ibc/app.
	// A node without a data folder (in memory) reports no storage.
	if info := call(t, a.URL, "GET", "", "node_info", "", "", nil).(map[string]any); info["nodeKey"] != a.NodeKey() || info["storage"] != nil {
		t.Errorf("node_info = %v", info)
	}
}
