// Package node wires an ibc node together in one process: storage
// (Pebble), execution, consensus and the HTTP API. It replaces the three
// Python processes (gunicorn, consensus_service.py, execution_service.py)
// and the Redis that connected them.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/consensus"
	"github.com/ouripoupko/ibc-go/dispatcher"
	"github.com/ouripoupko/ibc-go/execution"
)

// Config configures a Node.
type Config struct {
	// DataDir holds the Pebble database. Ignored if DB is set.
	DataDir string
	// DB is an already open database (tests use an in-memory one).
	DB ethdb.Database
	// Sender delivers requests to partner nodes. Default: HTTP.
	Sender common.Sender
}

// Node is a running ibc node.
type Node struct {
	db        ethdb.Database
	ownsDB    bool
	navigator *execution.Navigator
	consensus consensus.Protocol
	broker    *dispatcher.Broker
	handler   http.Handler
	done      chan struct{} // closed to end the event streams
	endOnce   sync.Once
	closeOnce sync.Once
	closeErr  error
}

// New opens the database, recovers unapplied records and assembles the node.
func New(cfg Config) (*Node, error) {
	n := &Node{db: cfg.DB, broker: dispatcher.NewBroker(), done: make(chan struct{})}
	if n.db == nil {
		db, err := common.OpenPebble(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		n.db, n.ownsDB = db, true
	}
	sender := cfg.Sender
	if sender == nil {
		sender = &common.HTTPSender{}
	}
	out := &output{broker: n.broker}
	n.navigator = execution.NewNavigator(execution.Config{DB: n.db, Sender: sender, Output: out})
	n.consensus = consensus.NewImmediate(n.navigator, n.broker)
	out.setConsensus(n.consensus)
	if err := n.navigator.Recover(); err != nil {
		return nil, errors.Join(fmt.Errorf("recover: %w", err), n.Close())
	}
	n.handler = dispatcher.New(n.navigator, n.consensus, n.broker, sender, n.done)
	return n, nil
}

// Handler returns the node's HTTP API.
func (n *Node) Handler() http.Handler { return n.handler }

// Close ends the event streams, applies what is already logged, and closes
// the database (if the node opened it). It is safe to call more than once.
func (n *Node) Close() error {
	n.endStreams()
	n.closeOnce.Do(func() {
		n.consensus.Close()
		n.navigator.Close()
		if n.ownsDB {
			n.closeErr = n.db.Close()
		}
	})
	return n.closeErr
}

func (n *Node) endStreams() { n.endOnce.Do(func() { close(n.done) }) }

// ListenAndServe serves the API on addr until ctx is cancelled, then shuts
// down gracefully and closes the node.
func (n *Node) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return n.Serve(ctx, ln)
}

// Serve is ListenAndServe on an existing listener.
func (n *Node) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           n.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: the event stream is a long-lived response.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	slog.Info("ibc node listening", "addr", ln.Addr().String())

	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
		// End the event streams first, so Shutdown does not wait for them.
		n.endStreams()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err = srv.Shutdown(shutdownCtx)
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, n.Close())
}

// output routes execution's outputs: replies to the event stream, join
// outcomes to consensus.
type output struct {
	broker *dispatcher.Broker
	mu     sync.Mutex
	proto  consensus.Protocol
}

func (o *output) setConsensus(p consensus.Protocol) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.proto = p
}

func (o *output) Publish(agent string, reply execution.Reply) { o.broker.Publish(agent, reply) }

func (o *output) Partner(agent string, record execution.Record) {
	o.mu.Lock()
	p := o.proto
	o.mu.Unlock()
	if p != nil {
		p.PartnerUpdate(agent, record)
	}
}
