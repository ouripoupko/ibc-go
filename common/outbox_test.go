package common

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	for _, step := range []string{"first", "second", "third"} {
		if err := outbox.Put(Request{Method: "PUT", URL: node.URL + "/ibc/app/bob/c1",
			Params: map[string]string{"action": "a2a_consent"}, JSON: map[string]any{"step": step}}); err != nil {
			t.Fatal(err)
		}
	}
	outbox.Close() // returns only after everything queued was sent
	outbox.Close() // idempotent

	got := node.received()
	if len(got) != 3 {
		t.Fatalf("received %d requests, want 3", len(got))
	}
	for i, step := range []string{"first", "second", "third"} {
		if got[i].Method != "PUT" || got[i].Action != "a2a_consent" || got[i].Body["step"] != step {
			t.Errorf("request %d = %+v, want step %s", i, got[i], step)
		}
	}
	if err := outbox.Put(Request{URL: node.URL}); !errors.Is(err, ErrOutboxClosed) {
		t.Fatalf("Put after Close: %v", err)
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
