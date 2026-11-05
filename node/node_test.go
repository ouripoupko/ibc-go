package node

import (
	"bufio"
	"bytes"
	"context"
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

	"github.com/ethereum/go-ethereum/core/rawdb"

	"github.com/ouripoupko/ibc-go/common"
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

func openStream(t *testing.T, base string, pairs ...string) *stream {
	t.Helper()
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		q.Add("agent", pairs[i])
		q.Add("contract", pairs[i+1])
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
	timeout := time.After(5 * time.Second)
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
	timeout := time.After(5 * time.Second)
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

func deployBody(t *testing.T, pid, address string) map[string]any {
	t.Helper()
	bin, err := os.ReadFile("../testdata/Registry.bin")
	if err != nil {
		t.Fatal(err)
	}
	abiJSON, err := os.ReadFile("../testdata/Registry.abi")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"name": "my-registry", "contract": "Registry.sol", "protocol": "BFT",
		"default_app": "https://app.example", "pid": pid, "address": address, "profile": "",
		"code": strings.TrimSpace(string(bin)), "abi": string(abiJSON),
		"constructor": map[string]any{"initialName": "registry", "start": 7},
	}
}

// The walkthrough of blockchain-api.md ("Putting it together"), on one node.
func TestClientAPI(t *testing.T) {
	a := startNode(t, Config{})
	const me = "alice"

	if got := call(t, a.URL, "GET", me, "is_exist_agent", "", "", nil); got != false {
		t.Fatalf("is_exist_agent before register = %v", got)
	}
	events := openStream(t, a.URL, me, "")
	ack := call(t, a.URL, "PUT", me, "register_agent", "", "", map[string]any{"address": a.URL})
	if ev := events.waitFor(t, ack); ev["reply"] != true || ev["agent"] != me || ev["contract"] != nil {
		t.Fatalf("register event = %v", ev)
	}
	if got := call(t, a.URL, "GET", me, "is_exist_agent", "", "", nil); got != true {
		t.Fatalf("is_exist_agent after register = %v", got)
	}

	contractID := call(t, a.URL, "PUT", me, "deploy_contract", "", "", deployBody(t, me, a.URL))
	ev := events.waitFor(t, contractID)
	if ev["reply"] != true || ev["action"] != "deploy_contract" || ev["contract"] != contractID {
		t.Fatalf("deploy event = %v", ev)
	}
	cid := contractID.(string)

	req := call(t, a.URL, "POST", me, "contract_write", cid, "setValue", map[string]any{"values": map[string]any{"value": 42}})
	if ev := events.waitFor(t, req); ev["reply"] != json.Number("8") || ev["state_root"] == "" {
		t.Fatalf("write event = %v", ev)
	}

	who := common.AgentAddress(me).Hex()
	if got := call(t, a.URL, "POST", me, "contract_read", cid, "getValue", map[string]any{"values": map[string]any{"who": who}}); got != json.Number("42") {
		t.Fatalf("contract_read getValue = %v", got)
	}
	if got := call(t, a.URL, "POST", me, "contract_read", cid, "nope", map[string]any{"values": map[string]any{}}); !strings.Contains(fmt.Sprint(got), "unknown method") {
		t.Fatalf("contract_read unknown method = %v", got)
	}
	if got := call(t, a.URL, "POST", me, "contract_read", "missing", "counter", map[string]any{"values": map[string]any{}}); fmt.Sprint(got) != "map[message:no such contract]" {
		t.Fatalf("contract_read missing contract = %v", got)
	}
	// A failing write answers on the stream with the revert reason.
	req = call(t, a.URL, "POST", me, "contract_write", cid, "setThenFail", map[string]any{"values": map[string]any{"value": 1}})
	if ev := events.waitFor(t, req); ev["reply"] != "execution reverted: setThenFail: rejected" {
		t.Fatalf("revert event = %v", ev)
	}

	list := call(t, a.URL, "GET", me, "get_contracts", "", "", nil).([]any)
	if len(list) != 1 {
		t.Fatalf("get_contracts = %v", list)
	}
	one := call(t, a.URL, "GET", me, "get_contract", cid, "", nil).(map[string]any)
	if one["id"] != cid || one["name"] != "my-registry" || one["abi"] == nil || one["evm_address"] == nil {
		t.Fatalf("get_contract = %v", one)
	}
	if got := call(t, a.URL, "GET", me, "get_contract", "missing", "", nil); fmt.Sprint(got) != "map[]" {
		t.Fatalf("get_contract missing = %v", got)
	}

	ledger := call(t, a.URL, "POST", me, "a2a_get_ledger", cid, "", map[string]any{"msg": map[string]any{"index": 2}}).(map[string]any)
	if len(ledger) != 2 || ledger["000000000000002"] == nil || ledger["000000000000003"] == nil {
		t.Fatalf("a2a_get_ledger from 2 = %v", ledger)
	}

	if got := call(t, a.URL, "PUT", me, "no_such_action", "", "", nil); !strings.Contains(fmt.Sprint(got), "unknown action") {
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
func TestJoinAcrossNodes(t *testing.T) {
	a := startNode(t, Config{})
	b := startNode(t, Config{})

	aliceEvents := openStream(t, a.URL, "alice", "")
	bobEvents := openStream(t, b.URL, "bob", "")
	call(t, a.URL, "PUT", "alice", "register_agent", "", "", map[string]any{"address": a.URL})
	call(t, b.URL, "PUT", "bob", "register_agent", "", "", map[string]any{"address": b.URL})
	cid := call(t, a.URL, "PUT", "alice", "deploy_contract", "", "", deployBody(t, "alice", a.URL))
	aliceEvents.waitFor(t, cid)

	ack := call(t, b.URL, "PUT", "bob", "join_contract", "", "", map[string]any{
		"contract": cid, "address": a.URL + "/", "agent": "alice", "profile": "",
	})
	if s, ok := ack.(string); !ok || len(s) != 64 {
		t.Fatalf("join_contract ack = %v", ack)
	}
	if ev := aliceEvents.waitAction(t, "a2a_connect"); ev["reply"] != false || ev["contract"] != cid {
		t.Fatalf("alice's a2a_connect event = %v", ev)
	}
	if ev := bobEvents.waitAction(t, "a2a_reply_join"); ev["reply"] != false || ev["agent"] != "bob" {
		t.Fatalf("bob's a2a_reply_join event = %v", ev)
	}
	// Alice's ledger has the deploy and the join request.
	ledger := call(t, a.URL, "POST", "alice", "a2a_get_ledger", cid.(string), "", map[string]any{"msg": map[string]any{"index": 0}}).(map[string]any)
	if len(ledger) != 2 {
		t.Fatalf("alice's ledger = %v", ledger)
	}
}

func TestStreamScopes(t *testing.T) {
	a := startNode(t, Config{})
	call(t, a.URL, "PUT", "alice", "register_agent", "", "", map[string]any{"address": a.URL})
	c1 := call(t, a.URL, "PUT", "alice", "deploy_contract", "", "", deployBody(t, "alice", a.URL)).(string)
	body := deployBody(t, "alice", a.URL)
	body["name"] = "second"
	c2 := call(t, a.URL, "PUT", "alice", "deploy_contract", "", "", body).(string)

	all := openStream(t, a.URL, "alice", "")
	only1 := openStream(t, a.URL, "alice", c1)
	other := openStream(t, a.URL, "carol", "")

	w2 := call(t, a.URL, "POST", "alice", "contract_write", c2, "setValue", map[string]any{"values": map[string]any{"value": 1}})
	w1 := call(t, a.URL, "POST", "alice", "contract_write", c1, "setValue", map[string]any{"values": map[string]any{"value": 1}})
	// Different contracts run in parallel, so the two events may come in either order.
	seen := map[any]bool{}
	for len(seen) < 2 {
		seen[all.waitAction(t, "contract_write")["request"]] = true
	}
	if !seen[w1] || !seen[w2] {
		t.Fatalf("all-contracts stream saw %v, want %v and %v", seen, w1, w2)
	}
	if ev := only1.waitAction(t, "contract_write"); ev["request"] != w1 {
		t.Fatalf("contract-scoped stream got %v, want only %v", ev, w1)
	}
	select {
	case ev := <-other.events:
		t.Fatalf("carol's stream got alice's event %v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDataSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a := startNode(t, Config{DataDir: dir})
	events := openStream(t, a.URL, "alice", "")
	call(t, a.URL, "PUT", "alice", "register_agent", "", "", map[string]any{"address": a.URL})
	cid := call(t, a.URL, "PUT", "alice", "deploy_contract", "", "", deployBody(t, "alice", a.URL))
	events.waitFor(t, cid)
	req := call(t, a.URL, "POST", "alice", "contract_write", cid.(string), "setValue", map[string]any{"values": map[string]any{"value": 5}})
	events.waitFor(t, req)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b := startNode(t, Config{DataDir: dir})
	if got := call(t, b.URL, "POST", "alice", "contract_read", cid.(string), "counter", map[string]any{"values": map[string]any{}}); got != json.Number("8") {
		t.Fatalf("counter after restart = %v", got)
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
	s := openStream(t, base, "alice", "")

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
