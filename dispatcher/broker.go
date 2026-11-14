package dispatcher

import (
	"log/slog"
	"sync"

	"github.com/ouripoupko/ibc-go/execution"
)

// Event is one message on the event stream: a reply plus the agent it
// belongs to.
type Event struct {
	execution.Reply
	Agent string `json:"agent"`
}

// Scope selects the events of one agent: all of them (Contract == "") or
// those of one contract.
type Scope struct {
	Agent    string
	Contract string
	// Allow, if set, further limits the events to those it accepts (an app
	// hears only about the contracts it has a permission for).
	Allow func(reply execution.Reply) bool
}

func (s Scope) matches(agent string, reply execution.Reply) bool {
	if s.Agent != agent {
		return false
	}
	if s.Allow != nil && (reply.Contract == nil || !s.Allow(reply)) {
		return false
	}
	return s.Contract == "" || (reply.Contract != nil && *reply.Contract == s.Contract)
}

// Broker fans replies out to event stream subscribers. It replaces Redis
// pub/sub. Publish never blocks: a subscriber that falls too far behind is
// disconnected (its channel is closed), and its client reconnects.
type Broker struct {
	mu   sync.Mutex
	subs map[*subscription]struct{}
}

type subscription struct {
	scopes []Scope
	ch     chan Event
}

// subscriberBuffer is how many events a slow subscriber may lag behind.
const subscriberBuffer = 256

// NewBroker returns an empty Broker.
func NewBroker() *Broker {
	return &Broker{subs: map[*subscription]struct{}{}}
}

// Subscribe returns a channel of the events matching any of scopes, and a
// function that ends the subscription. The channel is closed when the
// subscription ends, from either side.
func (b *Broker) Subscribe(scopes []Scope) (<-chan Event, func()) {
	sub := &subscription{scopes: scopes, ch: make(chan Event, subscriberBuffer)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	return sub.ch, func() { b.remove(sub) }
}

func (b *Broker) remove(sub *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[sub]; ok {
		delete(b.subs, sub)
		close(sub.ch)
	}
}

// Publish delivers a reply to every subscriber whose scopes match it.
func (b *Broker) Publish(agent string, reply execution.Reply) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		for _, scope := range sub.scopes {
			if !scope.matches(agent, reply) {
				continue
			}
			select {
			case sub.ch <- Event{Reply: reply, Agent: agent}:
			default:
				slog.Warn("event stream subscriber too slow, disconnecting", "agent", agent)
				delete(b.subs, sub)
				close(sub.ch)
			}
			break
		}
	}
}
