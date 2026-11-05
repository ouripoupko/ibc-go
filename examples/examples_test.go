// Package examples_test runs the calls shown in docs/usage.md against the
// example contracts, so the documented results stay true.
package examples_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/rawdb"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/node"
)

type client struct {
	t      *testing.T
	url    string
	agent  string
	events chan map[string]any
}

func newClient(t *testing.T, agent string) *client {
	t.Helper()
	n, err := node.New(node.Config{DB: rawdb.NewMemoryDatabase()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(n.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = n.Close() })
	c := &client{t: t, url: srv.URL, agent: agent, events: make(chan map[string]any, 100)}
	c.openStream()
	return c
}

func (c *client) openStream() {
	resp, err := http.Get(c.url + "/stream?agent=" + c.agent + "&contract=")
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
	c.await(c.request("PUT", "register_agent", "", "", map[string]any{"address": c.url}))
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
	ev := c.await(c.request("PUT", "deploy_contract", "", "", map[string]any{
		"name": name, "contract": name + ".sol", "protocol": "BFT", "default_app": "",
		"pid": c.agent, "address": c.url, "profile": "",
		"code": strings.TrimSpace(string(bin)), "abi": string(abiJSON), "constructor": ctor,
	}))
	return ev["contract"].(string), ev["reply"]
}

func (c *client) write(contract, method string, values map[string]any) any {
	c.t.Helper()
	return c.await(c.request("POST", "contract_write", contract, method, map[string]any{"values": values}))["reply"]
}

func (c *client) read(contract, method string, values map[string]any) any {
	c.t.Helper()
	return c.request("POST", "contract_read", contract, method, map[string]any{"values": values})
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
	c := newClient(t, "alice")
	c.register()
	counter, reply := c.deploy("Counter", map[string]any{"start": 10})
	expect(t, "deploy", reply, `true`)

	expect(t, "increment(5)", c.write(counter, "increment", map[string]any{"amount": 5}), `15`)
	expect(t, "increment(0)", c.write(counter, "increment", map[string]any{"amount": 0}),
		`"execution reverted: amount must be positive"`)
	expect(t, "count", c.read(counter, "count", nil), `15`)
	expect(t, "myContribution", c.read(counter, "myContribution", nil), `5`)
	expect(t, "owner", c.read(counter, "owner", nil), fmt.Sprintf("%q", common.AgentAddress("alice").Hex()))
	// The getter of a public mapping has an unnamed parameter: key "0".
	expect(t, "contributions", c.read(counter, "contributions", map[string]any{"0": common.AgentAddress("alice").Hex()}), `5`)
	// A read-only call of a write method changes nothing.
	expect(t, "read increment", c.read(counter, "increment", map[string]any{"amount": 100}), `115`)
	expect(t, "count after read", c.read(counter, "count", nil), `15`)
	expect(t, "reset", c.write(counter, "reset", nil), `null`)
	expect(t, "count after reset", c.read(counter, "count", nil), `0`)
	expect(t, "missing argument", c.read(counter, "increment", nil), `"increment: missing argument \"amount\""`)
	expect(t, "no such contract", c.read("nope", "count", nil), `{"message":"no such contract"}`)
}

func TestPoll(t *testing.T) {
	c := newClient(t, "alice")
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
	c := newClient(t, "alice")
	c.register()
	board, reply := c.deploy("Board", nil)
	expect(t, "deploy", reply, `true`)
	alice := common.AgentAddress("alice").Hex()

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
