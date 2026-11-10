// Package examples_test runs the calls shown in docs/usage.md against the
// example contracts, so the documented results stay true.
package examples_test

import (
	"bufio"
	"bytes"
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
	n, err := node.New(node.Config{DB: rawdb.NewMemoryDatabase()})
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
		"node": c.url, "nodeKey": c.nodeKey,
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
		AppKey: identity.PublicKey(appKey), CodeHash: codeHash, Read: read, Functions: functions, Nonce: identity.NewNonce()}, c.writer)
	if err != nil {
		c.t.Fatal(err)
	}
	body := map[string]any{"consent": consent}
	if len(functions) > 0 {
		grant, err := identity.CertifyGrant(identity.Grant{Agent: agent, NodeKey: ethcommon.HexToAddress(c.nodeKey),
			CodeHash: codeHash, Functions: functions, Nonce: identity.NewNonce()}, c.writer)
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
	bin, _ := os.ReadFile("build/Chat.bin")
	chatCode, err := identity.CodeHash(string(bin))
	if err != nil {
		t.Fatal(err)
	}
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
		CodeHash: chatCode, Read: true, Functions: []string{"post"}, Nonce: "n"}, c.writer)
	grant, _ := identity.CertifyGrant(identity.Grant{Agent: agent, NodeKey: ethcommon.HexToAddress(c.nodeKey),
		CodeHash: chatCode, Functions: []string{"post", "setTopic"}, Nonce: "n"}, c.writer)
	got := c.request("PUT", "register_app", "", "", map[string]any{"consent": consent, "grant": grant})
	if !strings.Contains(fmt.Sprint(got), "does not match") {
		t.Errorf("mismatched grant: %v", got)
	}
}
