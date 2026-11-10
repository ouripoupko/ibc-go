package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// Public contracts (docs/api.md, "Public reads"): an agent lets anyone read
// its copy of a contract. The agent's root key signs the choice (set_public);
// the reads (public_read) are unsigned, act for no one, and run with
// msg.sender the zero address. The contract decides what an unknown reader
// sees: it can refuse with require(msg.sender != address(0)).

const (
	actionSetPublic  = "set_public"
	actionPublicRead = "public_read"
)

// Limits on anonymous reads: a rate per client address, and how many run at once.
var (
	publicRate     = 10.0 // reads per second, per client address
	publicBurst    = 20.0
	publicParallel = 16
)

// errNotPublic answers both a contract that is not public and one the agent
// does not have, so that a stranger learns nothing of private contracts.
func errNotPublic(agent, contract string) error {
	return fmt.Errorf("agent %s has no public contract %s", agent, contract)
}

// setPublic records the agent's choice, signed by its root key.
func (d *Dispatcher) setPublic(record execution.Record, body []byte) (any, error) {
	var sp identity.SignedPublish
	if err := json.Unmarshal(body, &sp); err != nil {
		return nil, fmt.Errorf("set_public: %w", err)
	}
	if err := sp.Verify(); err != nil {
		return nil, fmt.Errorf("set_public: %w", err)
	}
	p := sp.Publish
	if p.Agent.Hex() != record.Agent {
		return nil, fmt.Errorf("publish of %s sent as %s", p.Agent.Hex(), record.Agent)
	}
	if record.Contract != "" && !strings.EqualFold(record.Contract, p.Contract) {
		return nil, fmt.Errorf("publish for contract %s sent to %s", p.Contract, record.Contract)
	}
	if time.UnixMilli(int64(p.Time)).After(time.Now().Add(queryWindow)) {
		return nil, errors.New("set_public: its time is in the future: check the clock")
	}
	err := d.navigator.SetPublic(record.Agent, p.Contract, p.Public, p.Time)
	if errors.Is(err, execution.ErrUnknownContract) {
		return nil, fmt.Errorf("agent %s has no contract %s", record.Agent, p.Contract)
	}
	if err != nil {
		return nil, err
	}
	return p.Public, nil
}

// publicRead serves an unsigned read of a public contract.
func (d *Dispatcher) publicRead(record execution.Record, query url.Values) (any, error) {
	public, err := d.navigator.IsPublic(record.Agent, record.Contract)
	if err != nil {
		return nil, err
	}
	if !public {
		return nil, errNotPublic(record.Agent, record.Contract)
	}
	if record.Method == "" {
		return nil, errors.New("public_read needs a method: /ibc/app/<agent>/<contract>/<method>")
	}
	var values map[string]any
	if raw := query.Get("values"); raw != "" {
		if err := execution.DecodeJSON([]byte(raw), &values); err != nil {
			return nil, fmt.Errorf("public_read values: %w", err)
		}
	}
	record.Message = execution.Message{Values: values}
	result, err := d.navigator.PublicRead(record.Agent, record)
	if errors.Is(err, execution.ErrUnknownContract) {
		return nil, errNotPublic(record.Agent, record.Contract)
	}
	if err != nil {
		var ce *common.ContractError
		if errors.As(err, &ce) || errors.Is(err, common.ErrNotImplemented) {
			return err.Error(), nil
		}
		return nil, err
	}
	return result.Value, nil
}

// limiter bounds anonymous reads: a token bucket per client address, and a
// number that may run at once.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	running chan struct{}
	swept   time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}, running: make(chan struct{}, publicParallel)}
}

// acquire takes a read for the client, or says why not. release ends it.
func (l *limiter) acquire(client string) (release func(), err error) {
	now := time.Now()
	l.mu.Lock()
	if now.Sub(l.swept) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.at) > time.Minute {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[client]
	if !ok {
		b = &bucket{tokens: publicBurst, at: now}
		l.buckets[client] = b
	}
	b.tokens = min(publicBurst, b.tokens+now.Sub(b.at).Seconds()*publicRate)
	b.at = now
	if b.tokens < 1 {
		l.mu.Unlock()
		return nil, errors.New("too many public reads: slow down")
	}
	b.tokens--
	l.mu.Unlock()
	select {
	case l.running <- struct{}{}:
		return func() { <-l.running }, nil
	default:
		return nil, errors.New("the node is busy with public reads: try again")
	}
}

// clientAddress is the address a request came from. Behind a proxy on the
// same machine (Caddy, in docs/deploy.md), it is the last address the proxy
// added to X-Forwarded-For.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if fwd := r.Header.Values("X-Forwarded-For"); len(fwd) > 0 {
			parts := strings.Split(fwd[len(fwd)-1], ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	return host
}
