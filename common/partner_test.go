package common

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

type received struct {
	Method, Path, Action string
	Body                 map[string]any
}

// partnerNode is a fake partner node that records what it receives.
type partnerNode struct {
	*httptest.Server
	mu   sync.Mutex
	got  []received
	resp string
}

func newPartnerNode(t *testing.T, resp string) *partnerNode {
	n := &partnerNode{resp: resp}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		n.mu.Lock()
		n.got = append(n.got, received{r.Method, r.URL.Path, r.URL.Query().Get("action"), body})
		n.mu.Unlock()
		if r.URL.Query().Get("action") == "fail" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, n.resp)
	}))
	t.Cleanup(n.Close)
	return n
}

func (n *partnerNode) received() []received {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]received(nil), n.got...)
}

func TestPartnerDirectRequests(t *testing.T) {
	node := newPartnerNode(t, `{"1":{"action":"deploy_contract"}}`)
	p := NewPartner(node.URL, "bob", "http://alice.example", "alice", &HTTPSender{}, nil)
	ctx := context.Background()

	if err := p.Connect(ctx, "c1", map[string]any{"name": "Alice"}); err != nil {
		t.Fatal(err)
	}
	ledger, err := p.GetLedger(ctx, "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(ledger) != node.resp {
		t.Errorf("GetLedger = %s", ledger)
	}

	got := node.received()
	want := []received{
		{"PUT", "/ibc/app/bob/c1", "a2a_connect", map[string]any{"from": "alice", "to": "bob",
			"msg": map[string]any{"address": "http://alice.example", "pid": "alice", "profile": map[string]any{"name": "Alice"}}}},
		{"POST", "/ibc/app/bob/c1", "a2a_get_ledger", map[string]any{"from": "alice", "to": "bob",
			"msg": map[string]any{"index": 0.0, "pid": "alice"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("received %+v\nwant     %+v", got, want)
	}
}

func TestHTTPSenderReportsErrorStatus(t *testing.T) {
	node := newPartnerNode(t, "")
	_, err := (&HTTPSender{}).Send(context.Background(),
		Request{Method: "PUT", URL: node.URL + "/x", Params: map[string]string{"action": "fail"}})
	if err == nil {
		t.Fatal("no error for status 500")
	}
}

func TestOutboxSendsInOrderAndFlushesOnClose(t *testing.T) {
	node := newPartnerNode(t, "")
	outbox := NewOutbox(&HTTPSender{})
	p := NewPartner(node.URL, "bob", "http://alice.example", "alice", nil, outbox)

	if err := p.ReplyJoin("c1", true); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"PRE_PREPARE", "PREPARE", "COMMIT"} {
		if err := p.Consent("c1", step, map[string]any{"n": 1}); err != nil {
			t.Fatal(err)
		}
	}
	outbox.Close() // returns only after everything queued was sent
	outbox.Close() // idempotent

	got := node.received()
	if len(got) != 4 {
		t.Fatalf("received %d requests, want 4", len(got))
	}
	if got[0].Action != "a2a_reply_join" || got[0].Body["msg"].(map[string]any)["status"] != true {
		t.Errorf("first request = %+v", got[0])
	}
	for i, step := range []string{"PRE_PREPARE", "PREPARE", "COMMIT"} {
		r := got[i+1]
		if r.Method != "PUT" || r.Action != "a2a_consent" || r.Body["msg"].(map[string]any)["step"] != step {
			t.Errorf("request %d = %+v, want consent %s", i+1, r, step)
		}
	}
	if err := p.Consent("c1", "PREPARE", nil); !errors.Is(err, ErrOutboxClosed) {
		t.Fatalf("Consent after Close: %v", err)
	}
}

// blockingSender holds every send until released, to show that Put never
// waits for a slow partner.
type blockingSender struct {
	release chan struct{}
	RecordingSender
}

func (s *blockingSender) Send(ctx context.Context, req Request) ([]byte, error) {
	<-s.release
	return s.RecordingSender.Send(ctx, req)
}

func TestOutboxPutDoesNotBlock(t *testing.T) {
	s := &blockingSender{release: make(chan struct{})}
	outbox := NewOutbox(s)
	for i := 0; i < 10_000; i++ {
		if err := outbox.Put(Request{URL: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	close(s.release)
	outbox.Close()
	if n := len(s.Requests()); n != 10_000 {
		t.Fatalf("sent %d, want 10000", n)
	}
}
