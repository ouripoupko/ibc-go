package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
)

// Request is an HTTP request to a partner's node.
type Request struct {
	Method string            `json:"method"`
	URL    string            `json:"url"`
	Params map[string]string `json:"params"`
	JSON   any               `json:"json"`
}

// Sender delivers a request.
type Sender interface {
	Send(ctx context.Context, req Request) ([]byte, error)
}

// HTTPSender sends requests over HTTP with a JSON body and returns the
// response body. A non-2xx status is an error.
type HTTPSender struct {
	Client *http.Client // nil means a client with a 30 second timeout
}

var defaultClient = &http.Client{Timeout: 30 * time.Second}

func (s *HTTPSender) Send(ctx context.Context, req Request) ([]byte, error) {
	u, err := url.Parse(req.URL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for k, v := range req.Params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	body, err := json.Marshal(req.JSON)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return respBody, fmt.Errorf("%s %s: %s", req.Method, req.URL, resp.Status)
	}
	return respBody, nil
}

// RecordingSender keeps requests instead of sending them, for tests.
type RecordingSender struct {
	mu       sync.Mutex
	requests []Request
}

func (s *RecordingSender) Send(_ context.Context, req Request) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	return nil, nil
}

// Requests returns the requests sent so far, in order.
func (s *RecordingSender) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ErrOutboxClosed is returned when a request is queued after Close.
var ErrOutboxClosed = errors.New("outbox is closed")

// Outbox sends queued requests one at a time, in order, from its own
// goroutine. It replaces the Python Queue plus sender thread. The queue is
// unbounded, so a slow or unreachable partner never blocks the caller.
// Failed sends are logged and dropped, as in Python.
type Outbox struct {
	sender Sender
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []Request
	closed bool
	done   chan struct{}
}

// NewOutbox starts an outbox that delivers through sender.
func NewOutbox(sender Sender) *Outbox {
	o := &Outbox{sender: sender, done: make(chan struct{})}
	o.cond = sync.NewCond(&o.mu)
	go o.run()
	return o
}

// Put queues a request.
func (o *Outbox) Put(req Request) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrOutboxClosed
	}
	o.queue = append(o.queue, req)
	o.cond.Signal()
	return nil
}

// Close stops the outbox after every queued request has been sent.
func (o *Outbox) Close() {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		o.cond.Signal()
	}
	o.mu.Unlock()
	<-o.done
}

func (o *Outbox) run() {
	defer close(o.done)
	for {
		o.mu.Lock()
		for len(o.queue) == 0 && !o.closed {
			o.cond.Wait()
		}
		if len(o.queue) == 0 {
			o.mu.Unlock()
			return
		}
		req := o.queue[0]
		o.queue[0] = Request{}
		o.queue = o.queue[1:]
		o.mu.Unlock()
		if _, err := o.sender.Send(context.Background(), req); err != nil {
			slog.Warn("send to partner failed", "url", req.URL, "err", err)
		}
	}
}
