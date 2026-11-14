// Package examples_test runs the calls shown in docs/usage.md against the
// example contracts, so the documented results stay true.
package examples_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ouripoupko/ibc-go/compiler"
	"github.com/ouripoupko/ibc-go/identity"
	"github.com/ouripoupko/ibc-go/node"
)

// client talks to a node as the user, signing with the user's root key (the
// passkey's role in a browser).
type client struct {
	t       *testing.T
	url     string
	nodeKey string
	key     *ecdsa.PrivateKey  // the root key
	writer  identity.KeySigner // the root key, as a signer
	agent   string             // the root key's address
	events  chan map[string]any
}

func newClient(t *testing.T) *client {
	t.Helper()
	// No compiler in the tests: a joined contract's source counts as checked.
	n, err := node.New(node.Config{DB: rawdb.NewMemoryDatabase(),
		CheckSource: func(context.Context, string, json.RawMessage, string) (string, string) { return compiler.Verified, "" }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(n.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = n.Close() })
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, url: srv.URL, nodeKey: n.NodeKey(), key: key, writer: identity.Secp256k1Signer{Key: key}, agent: identity.Address(key).Hex(),
		events: make(chan map[string]any, 100)}
	c.openStream()
	return c
}

// sign signs a transaction of the user.
func (c *client) sign(kind, contract string, body any) identity.SignedTx {
	c.t.Helper()
	raw, _ := json.Marshal(body)
	stx, err := identity.SignTx(identity.Transaction{Contract: contract, Kind: kind, Agent: identity.Address(c.key),
		Nonce: identity.NewNonce(), Body: string(raw)}, c.writer)
	if err != nil {
		c.t.Fatal(err)
	}
	return stx
}

// query signs a query of the user for its own agent's copy.
func (c *client) query(key identity.KeySigner, agent ethcommon.Address, action, contract string, body any) identity.SignedQuery {
	c.t.Helper()
	raw := ""
	if body != nil {
		b, _ := json.Marshal(body)
		raw = string(b)
	}
	sq, err := identity.SignQuery(identity.Query{Agent: agent, Action: action, Target: c.agent,
		Contract: contract, Body: raw, Time: uint64(time.Now().Unix()), Nonce: identity.NewNonce()}, key)
	if err != nil {
		c.t.Fatal(err)
	}
	return sq
}

func (c *client) openStream() {
	auth, _ := json.Marshal(c.query(c.writer, identity.Address(c.key), "stream", "", nil))
	resp, err := http.Get(c.url + "/stream?" + url.Values{"auth": {string(auth)}}.Encode())
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { resp.Body.Close() })
	connected := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		first := true
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			if first {
				close(connected)
				first = false
			}
			if data != "" {
				var ev map[string]any
				dec := json.NewDecoder(strings.NewReader(data))
				dec.UseNumber()
				if dec.Decode(&ev) == nil {
					c.events <- ev
				}
			}
		}
	}()
	<-connected
}

func (c *client) request(verb, action, contract, method string, body any) any {
	c.t.Helper()
	path := c.agent
	if contract != "" {
		path += "/" + contract
	}
	if method != "" {
		path += "/" + method
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(verb, c.url+"/ibc/app/"+path+"?action="+action, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		c.t.Fatalf("%s: %v (%s)", action, err, data)
	}
	return out
}

func (c *client) await(request any) map[string]any {
	c.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-c.events:
			if ev["request"] == request {
				return ev
			}
		case <-timeout:
			c.t.Fatalf("no event for %v", request)
		}
	}
}

func (c *client) register() {
	sr, err := identity.SignRegistration(identity.Registration{Agent: identity.Address(c.key), Node: c.url,
		NodeKey: ethcommon.HexToAddress(c.nodeKey), Nonce: identity.NewNonce()}, identity.Secp256k1Signer{Key: c.key})
	if err != nil {
		c.t.Fatal(err)
	}
	c.await(c.request("PUT", "register_agent", "", "", sr))
}

// deploy returns the contract id and the reply of the deploy event.
func (c *client) deploy(name string, ctor map[string]any) (string, any) {
	c.t.Helper()
	bin, err := os.ReadFile("build/" + name + ".bin")
	if err != nil {
		c.t.Fatal(err)
	}
	abiJSON, err := os.ReadFile("build/" + name + ".abi")
	if err != nil {
		c.t.Fatal(err)
	}
	ev := c.await(c.request("PUT", "deploy_contract", "", "", c.sign(identity.KindDeploy, "", map[string]any{
		"name": name, "contract": name + ".sol", "default_app": "", "profile": "",
		"code": strings.TrimSpace(string(bin)), "abi": json.RawMessage(abiJSON), "constructor": ctor,
		"node": c.url, "nodeKey": c.nodeKey, "source": source(c.t, name),
		"compiler": map[string]any{"version": "0.8.38+commit.1835d247.Emscripten.clang", "settings": compiler.Settings},
	})))
	return ev["contract"].(string), ev["reply"]
}

func (c *client) write(contract, method string, values map[string]any) any {
	c.t.Helper()
	stx := c.sign(identity.KindCall, contract, map[string]any{"method": method, "values": values})
	return c.await(c.request("POST", "contract_write", contract, method, stx))["reply"]
}

// connectApp records the user's consent for an app's key, for contracts of
// codeHash: reads without asking if read, and the functions the node may sign
// silently (with the grant for this node, signed by the root).
func (c *client) connectApp(appKey *ecdsa.PrivateKey, codeHash ethcommon.Hash, read bool, functions ...string) any {
	c.t.Helper()
	agent := identity.Address(c.key)
	consent, err := identity.SignConsent(identity.Consent{Agent: agent, App: "https://app.example",
		AppKey: identity.PublicKey(appKey), SourceHash: codeHash, Read: read, Functions: functions, Nonce: identity.NewNonce()}, c.writer)
	if err != nil {
		c.t.Fatal(err)
	}
	body := map[string]any{"consent": consent}
	if len(functions) > 0 {
		grant, err := identity.CertifyGrant(identity.Grant{Agent: agent, NodeKey: ethcommon.HexToAddress(c.nodeKey),
			SourceHash: codeHash, Functions: functions, Nonce: identity.NewNonce()}, c.writer)
		if err != nil {
			c.t.Fatal(err)
		}
		body["grant"] = grant
	}
	return c.request("PUT", "register_app", "", "", body)
}

// appWrite asks the node, as the app, to call method silently; the node signs
// it if the user allowed it.
func (c *client) appWrite(appKey *ecdsa.PrivateKey, contract, method string, values map[string]any) any {
	c.t.Helper()
	reply := c.request("POST", "contract_write", contract, method,
		c.query(identity.P256Signer{Key: appKey}, identity.Address(c.key), "contract_write", contract,
			map[string]any{"method": method, "values": values}))
	if _, isID := reply.(string); !isID {
		return reply // refused: {"message": ...}
	}
	return c.await(reply)["reply"]
}

// appRead reads as the app.
func (c *client) appRead(appKey *ecdsa.PrivateKey, contract, method string, values map[string]any) any {
	c.t.Helper()
	return c.readAs(identity.P256Signer{Key: appKey}, identity.Address(c.key), contract, method, values)
}

func (c *client) read(contract, method string, values map[string]any) any {
	c.t.Helper()
	return c.readAs(c.writer, identity.Address(c.key), contract, method, values)
}

// readAs reads the user's copy of a contract, signed by key.
func (c *client) readAs(key identity.KeySigner, agent ethcommon.Address, contract, method string, values map[string]any) any {
	c.t.Helper()
	if values == nil {
		values = map[string]any{}
	}
	return c.request("POST", "contract_read", contract, method,
		c.query(key, agent, "contract_read", contract, map[string]any{"method": method, "values": values}))
}

// expect compares a reply to the JSON shown in the docs.
func expect(t *testing.T, what string, got any, wantJSON string) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != wantJSON {
		t.Errorf("%s = %s, want %s", what, gotJSON, wantJSON)
	}
}

func TestCounter(t *testing.T) {
	c := newClient(t)
	c.register()
	counter, reply := c.deploy("Counter", map[string]any{"start": 10})
	expect(t, "deploy", reply, `true`)

	expect(t, "increment(5)", c.write(counter, "increment", map[string]any{"amount": 5}), `15`)
	expect(t, "increment(0)", c.write(counter, "increment", map[string]any{"amount": 0}),
		`"execution reverted: amount must be positive"`)
	expect(t, "count", c.read(counter, "count", nil), `15`)
	expect(t, "myContribution", c.read(counter, "myContribution", nil), `5`)
	expect(t, "owner", c.read(counter, "owner", nil), fmt.Sprintf("%q", c.agent))
	// The getter of a public mapping has an unnamed parameter: key "0".
	expect(t, "contributions", c.read(counter, "contributions", map[string]any{"0": c.agent}), `5`)
	// A read-only call of a write method changes nothing.
	// A read runs only view and pure functions.
	expect(t, "read increment", c.read(counter, "increment", map[string]any{"amount": 100}),
		`"increment changes state: send it as a transaction (contract_write)"`)
	expect(t, "count after read", c.read(counter, "count", nil), `15`)
	expect(t, "reset", c.write(counter, "reset", nil), `null`)
	expect(t, "count after reset", c.read(counter, "count", nil), `0`)
	expect(t, "missing argument", c.write(counter, "increment", nil), `"increment: missing argument \"amount\""`)
	expect(t, "no such contract", c.read("nope", "count", nil), `{"message":"no such contract"}`)
}

func TestPoll(t *testing.T) {
	c := newClient(t)
	c.register()
	_, reply := c.deploy("Poll", map[string]any{"text": "Lunch?", "choices": []string{"only one"}})
	expect(t, "deploy with one option", reply, `"execution reverted: a poll needs at least two options"`)

	poll, reply := c.deploy("Poll", map[string]any{"text": "Lunch?", "choices": []string{"pizza", "salad", "soup"}})
	expect(t, "deploy", reply, `true`)
	expect(t, "vote(1)", c.write(poll, "vote", map[string]any{"option": 1}), `1`)
	expect(t, "vote again", c.write(poll, "vote", map[string]any{"option": 2}), `"execution reverted: AlreadyVoted(1)"`)
	expect(t, "vote(7)", c.write(poll, "vote", map[string]any{"option": 7}), `"execution reverted: NoSuchOption(7, 3)"`)
	expect(t, "results", c.read(poll, "results", nil), `["Lunch?",["pizza","salad","soup"],[0,1,0]]`)
	expect(t, "myVote", c.read(poll, "myVote", nil), `[true,1]`)
	expect(t, "voters", c.read(poll, "voters", nil), `1`)
}

func TestBoard(t *testing.T) {
	c := newClient(t)
	c.register()
	board, reply := c.deploy("Board", nil)
	expect(t, "deploy", reply, `true`)
	alice := c.agent

	expect(t, "post", c.write(board, "post", map[string]any{"text": "hello ibc"}), `0`)
	expect(t, "post", c.write(board, "post", map[string]any{"text": "second post"}), `1`)
	expect(t, "empty post", c.write(board, "post", map[string]any{"text": ""}), `"execution reverted: empty post"`)

	post, ok := c.read(board, "getPost", map[string]any{"id": 0}).(map[string]any)
	if !ok || post["author"] != alice || post["text"] != "hello ibc" || post["time"] == nil {
		t.Errorf("getPost(0) = %v", post)
	}
	latest, ok := c.read(board, "latest", map[string]any{"limit": 5}).([]any)
	if !ok || len(latest) != 2 || latest[0].(map[string]any)["text"] != "second post" {
		t.Errorf("latest(5) = %v", latest)
	}
	expect(t, "myPosts", c.read(board, "myPosts", nil), `[0,1]`)
	expect(t, "count", c.read(board, "count", nil), `2`)
	expect(t, "whoAmI", c.read(board, "whoAmI", nil), fmt.Sprintf("%q", alice))
	expect(t, "getPost(9)", c.read(board, "getPost", map[string]any{"id": 9}), `"execution reverted: no such post"`)
}

// Chat, used by an app: the user lets the app post and like silently (the
// node signs, under the user's grant); the topic needs the user's root key.
func TestChat(t *testing.T) {
	c := newClient(t)
	c.register()
	chat, reply := c.deploy("Chat", map[string]any{"initialTopic": "lunch"})
	expect(t, "deploy", reply, `true`)
	chatCode := identity.SourceHash(source(t, "Chat"))
	app, _ := identity.NewP256Key()

	// Before the user connects the app, it can do nothing.
	if got := c.appRead(app, chat, "topic", nil); !strings.Contains(fmt.Sprint(got), "not agent") {
		t.Errorf("read by an unknown app: %v", got)
	}
	expect(t, "connect the app", c.connectApp(app, chatCode, true, "post", "like"), `true`)

	expect(t, "silent post", c.appWrite(app, chat, "post", map[string]any{"text": "hi"}), `0`)
	expect(t, "silent like", c.appWrite(app, chat, "like", map[string]any{"id": 0}), `1`)
	expect(t, "silent read", c.appRead(app, chat, "topic", nil), `"lunch"`)
	expect(t, "silent setTopic", c.appWrite(app, chat, "setTopic", map[string]any{"newTopic": "dinner"}),
		`{"message":"calling setTopic needs your approval on your node's page"}`)
	expect(t, "setTopic signed by the root", c.write(chat, "setTopic", map[string]any{"newTopic": "dinner"}), `null`)
	expect(t, "topic", c.read(chat, "topic", nil), `"dinner"`)

	// The app's permissions are for Chat's code: other contracts need approval.
	counter, _ := c.deploy("Counter", map[string]any{"start": 1})
	expect(t, "app writes to Counter", c.appWrite(app, counter, "increment", map[string]any{"amount": 1}),
		`{"message":"calling increment needs your approval on your node's page"}`)
	expect(t, "app reads Counter", c.appRead(app, counter, "count", nil),
		`{"message":"reading this contract needs your approval on your node's page"}`)

	// Without silent reads, reading asks too.
	expect(t, "reads off", c.connectApp(app, chatCode, false, "post"), `true`)
	expect(t, "read with reads off", c.appRead(app, chat, "topic", nil),
		`{"message":"reading this contract needs your approval on your node's page"}`)
	expect(t, "post still silent", c.appWrite(app, chat, "post", map[string]any{"text": "again"}), `1`)

	// A grant names functions the consent does not: refused.
	bad, _ := identity.NewP256Key()
	agent := identity.Address(c.key)
	consent, _ := identity.SignConsent(identity.Consent{Agent: agent, App: "https://app.example", AppKey: identity.PublicKey(bad),
		SourceHash: chatCode, Read: true, Functions: []string{"post"}, Nonce: "n"}, c.writer)
	grant, _ := identity.CertifyGrant(identity.Grant{Agent: agent, NodeKey: ethcommon.HexToAddress(c.nodeKey),
		SourceHash: chatCode, Functions: []string{"post", "setTopic"}, Nonce: "n"}, c.writer)
	got := c.request("PUT", "register_app", "", "", map[string]any{"consent": consent, "grant": grant})
	if !strings.Contains(fmt.Sprint(got), "does not match") {
		t.Errorf("mismatched grant: %v", got)
	}
}

// getContract returns the user's summary of a contract.
func (c *client) getContract(contract string) map[string]any {
	c.t.Helper()
	auth, _ := json.Marshal(c.query(c.writer, identity.Address(c.key), "get_contract", contract, nil))
	resp, err := http.Get(c.url + "/ibc/app/" + c.agent + "/" + contract + "?" +
		url.Values{"action": {"get_contract"}, "auth": {string(auth)}}.Encode())
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// source is an example contract's Solidity source.
func source(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile("contracts/" + name + ".sol")
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// joinAsGuest asks host, on its node, to let the user join as a guest a
// contract of the example name; it returns the reply of the join.
func (c *client) joinAsGuest(host *client, contract, name string) any {
	c.t.Helper()
	stx := c.sign(identity.KindJoin, contract, map[string]any{"node": c.url, "nodeKey": c.nodeKey,
		"guest": true, "host": host.agent, "sourceHash": identity.SourceHash(source(c.t, name)).Hex()})
	id := c.request("PUT", "join_contract", "", "", map[string]any{"tx": stx.Tx, "signature": stx.Signature,
		"partner": map[string]any{"address": host.url, "agent": host.agent}})
	return c.await(id)["reply"]
}

// A guest of Chat, on another node: it joins without taking part in
// consensus, and its node sends its posts and reads to the host's node.
func TestChatGuest(t *testing.T) {
	alice, bob := newClient(t), newClient(t)
	alice.register()
	bob.register()
	chat, _ := alice.deploy("Chat", map[string]any{"initialTopic": "lunch"})
	expect(t, "alice posts", alice.write(chat, "post", map[string]any{"text": "hi"}), `0`)

	expect(t, "bob joins as a guest", bob.joinAsGuest(alice, chat, "Chat"), `true`)
	if s := bob.getContract(chat); s["role"] != "guest" || s["name"] != "Chat" || len(s["hosts"].([]any)) != 1 {
		t.Errorf("bob's contract = %v", s)
	}
	if s := alice.getContract(chat); s["role"] != "partner" {
		t.Errorf("alice's contract = %v", s)
	}

	// Bob posts and reads through alice's node, as himself.
	expect(t, "bob posts", bob.write(chat, "post", map[string]any{"text": "hello from a guest"}), `1`)
	expect(t, "bob reads", bob.read(chat, "count", nil), `2`)
	latest := alice.read(chat, "latest", map[string]any{"limit": 1}).([]any)[0].(map[string]any)
	if latest["author"] != bob.agent || latest["text"] != "hello from a guest" {
		t.Errorf("latest = %v", latest)
	}
	expect(t, "a guest's revert", bob.read(chat, "latest", map[string]any{}), `"latest: missing argument \"limit\""`)

	// Bob hears alice's posts.
	stx := alice.sign(identity.KindCall, chat, map[string]any{"method": "post", "values": map[string]any{"text": "welcome"}})
	id := alice.request("POST", "contract_write", chat, "post", stx)
	expect(t, "bob hears alice's post", bob.await(id)["reply"], `2`)

	// Bob's app posts silently: bob's node signs, under bob's grant.
	chatCode := identity.SourceHash(source(t, "Chat"))
	app, _ := identity.NewP256Key()
	expect(t, "connect bob's app", bob.connectApp(app, chatCode, true, "post"), `true`)
	expect(t, "silent post by a guest", bob.appWrite(app, chat, "post", map[string]any{"text": "from the app"}), `3`)
	expect(t, "the app reads", bob.appRead(app, chat, "count", nil), `4`)

	// A contract without approve_guest takes no guests.
	counter, _ := alice.deploy("Counter", map[string]any{"start": 1})
	expect(t, "guest of Counter", bob.joinAsGuest(alice, counter, "Counter"), `false`)
	expect(t, "no Counter for bob", bob.getContract(counter), `{}`)

	// Bob leaves: his node forgets the contract.
	leave := bob.sign(identity.KindLeave, chat, map[string]any{"member": bob.agent})
	expect(t, "bob leaves", bob.await(bob.request("PUT", "leave_contract", chat, "", leave))["reply"], `true`)
	expect(t, "bob's read after leaving", bob.read(chat, "count", nil), `{"message":"no such contract"}`)
	expect(t, "alice's count", alice.read(chat, "count", nil), `4`)
}

// publicRead is anyone's read of a public contract: no key, no signature.
func publicRead(t *testing.T, nodeURL, agent, contract, method string, values map[string]any) any {
	t.Helper()
	raw, _ := json.Marshal(values)
	resp, err := http.Get(nodeURL + "/ibc/app/" + agent + "/" + contract + "/" + method + "?" +
		url.Values{"action": {"public_read"}, "values": {string(raw)}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// setPublic makes the user's copy of a contract public, or not, signed by the root.
func (c *client) setPublic(contract string, public bool) any {
	c.t.Helper()
	sp, err := identity.SignPublish(identity.Publish{Agent: identity.Address(c.key), Contract: contract, Public: public,
		Time: uint64(time.Now().UnixMilli()), Nonce: identity.NewNonce()}, c.writer)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.request("PUT", "set_public", contract, "", sp)
}

// A public Chat: anyone reads its topic and its number of messages, without
// signing in; the messages themselves need a signed read.
func TestChatPublic(t *testing.T) {
	alice, bob := newClient(t), newClient(t)
	alice.register()
	bob.register()
	chat, _ := alice.deploy("Chat", map[string]any{"initialTopic": "lunch"})
	expect(t, "alice posts", alice.write(chat, "post", map[string]any{"text": "hi"}), `0`)

	expect(t, "private by default", publicRead(t, alice.url, alice.agent, chat, "topic", nil),
		fmt.Sprintf(`{"message":"agent %s has no public contract %s"}`, alice.agent, chat))
	expect(t, "alice makes it public", alice.setPublic(chat, true), `true`)
	expect(t, "anyone reads the topic", publicRead(t, alice.url, alice.agent, chat, "topic", nil), `"lunch"`)
	expect(t, "anyone counts", publicRead(t, alice.url, alice.agent, chat, "count", nil), `1`)
	expect(t, "but not the messages", publicRead(t, alice.url, alice.agent, chat, "latest", map[string]any{"limit": 1}),
		`"execution reverted: sign in to read the messages"`)
	expect(t, "nor posts", publicRead(t, alice.url, alice.agent, chat, "post", map[string]any{"text": "spam"}),
		`"post changes state: send it as a transaction (contract_write)"`)
	if latest := alice.read(chat, "latest", map[string]any{"limit": 1}).([]any); len(latest) != 1 {
		t.Errorf("alice's signed read of latest = %v", latest)
	}

	// A guest keeps no copy, so has none to make public.
	expect(t, "bob joins as a guest", bob.joinAsGuest(alice, chat, "Chat"), `true`)
	expect(t, "a guest's publish", bob.setPublic(chat, true), `{"message":"a guest has no copy of the contract to make public"}`)

	expect(t, "alice makes it private", alice.setPublic(chat, false), `false`)
	expect(t, "private again", publicRead(t, alice.url, alice.agent, chat, "count", nil),
		fmt.Sprintf(`{"message":"agent %s has no public contract %s"}`, alice.agent, chat))
}
