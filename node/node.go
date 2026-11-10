// Package node wires an ibc node together in one process: storage
// (Pebble), execution, consensus and the HTTP API. It replaces the three
// Python processes (gunicorn, consensus_service.py, execution_service.py)
// and the Redis that connected them.
package node

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/compiler"
	"github.com/ouripoupko/ibc-go/consensus"
	"github.com/ouripoupko/ibc-go/consensus/cordial"
	"github.com/ouripoupko/ibc-go/dispatcher"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
	"github.com/ouripoupko/ibc-go/web"
)

// Config configures a Node.
type Config struct {
	// DataDir holds the Pebble database and the node key (file "nodekey").
	DataDir string
	// DB is an already open database (tests use an in-memory one); then the
	// node does not open or close one.
	DB ethdb.Database
	// NodeKey is the node's block-signing key. Nil: read it from DataDir,
	// creating it on first start (or a fresh key, if DataDir is empty).
	NodeKey *ecdsa.PrivateKey
	// Sender delivers requests to other nodes. Default: HTTP.
	Sender common.Sender
	// Params are the consensus rules; zero for the defaults.
	Params cordial.Params
	// Solc is the Solidity compiler command, for compiling apps' source
	// (for example ["solc"]); empty: the node cannot compile.
	Solc []string
}

// Node is a running ibc node.
type Node struct {
	db        ethdb.Database
	ownsDB    bool
	navigator *execution.Navigator
	consensus *consensus.Router
	nodeKey   *ecdsa.PrivateKey
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
	n.nodeKey = cfg.NodeKey
	if n.nodeKey == nil {
		var err error
		if cfg.DataDir != "" {
			if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
				return nil, err
			}
			n.nodeKey, err = identity.LoadOrCreateNodeKey(filepath.Join(cfg.DataDir, "nodekey"))
		} else {
			n.nodeKey, err = crypto.GenerateKey()
		}
		if err != nil {
			return nil, errors.Join(err, n.closeDB())
		}
	}
	sender := cfg.Sender
	if sender == nil {
		sender = &common.HTTPSender{}
	}
	out := &output{broker: n.broker}
	n.navigator = execution.NewNavigator(execution.Config{DB: n.db, Output: out})
	n.consensus = consensus.NewRouter(consensus.RouterConfig{
		NodeKey: n.nodeKey, DB: n.db, Sender: sender, Execution: n.navigator,
		Publisher: n.broker, Params: cfg.Params,
	})
	out.setConsensus(n.consensus)
	if err := n.navigator.Recover(); err != nil {
		return nil, errors.Join(fmt.Errorf("recover: %w", err), n.Close())
	}
	if err := n.consensus.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start consensus: %w", err), n.Close())
	}
	data := &storage{}
	if cfg.DB == nil {
		data.dir = cfg.DataDir
	}
	var solc *compiler.Solc
	if len(cfg.Solc) > 0 {
		solc = &compiler.Solc{Command: cfg.Solc}
	}
	api := dispatcher.New(n.navigator, n.consensus, n.broker, dispatcher.Options{
		NodeKey: n.nodeKey, Storage: data.Size, Compiler: solc,
		Apps: common.NewKVStore(n.db).Collection(dispatcher.AppsCollection),
	}, n.done)
	mux := http.NewServeMux()
	mux.Handle("/ibc/", api)
	mux.Handle("/stream", api)
	mux.Handle("/", web.Handler()) // the web interface
	n.handler = mux
	slog.Info("node key", "address", identity.Address(n.nodeKey).Hex())
	return n, nil
}

// Handler returns the node's HTTP API and its web interface.
func (n *Node) Handler() http.Handler { return n.handler }

// NodeKey returns the address of the node's block-signing key.
func (n *Node) NodeKey() string { return identity.Address(n.nodeKey).Hex() }

func (n *Node) closeDB() error {
	if n.ownsDB {
		return n.db.Close()
	}
	return nil
}

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

// output routes execution's outputs: replies to the event stream, decisions
// and progress to consensus.
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

func (o *output) protocol() consensus.Protocol {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.proto
}

func (o *output) Publish(agent string, reply execution.Reply) {
	o.broker.Publish(agent, reply)
	if p := o.protocol(); p != nil {
		p.Published(agent, reply) // to the contract's guests
	}
}

func (o *output) Decided(agent, contract string, record execution.Record, approved bool) {
	if p := o.protocol(); p != nil {
		p.Decided(agent, contract, record, approved)
	}
}

func (o *output) Applied(agent, contract string, index uint64, stateRoot string) {
	if p := o.protocol(); p != nil {
		p.Applied(agent, contract, index, stateRoot)
	}
}
