package consensus

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/consensus/cordial"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// Guest partners (docs/consensus.md, §11). A guest joins a contract like a
// partner, but takes no part in its consensus: the guest's node keeps no
// state, and sends the guest's transactions and reads to a host, one of the
// contract's members. The hosts order the guest's transactions with their
// own, and the host the guest joined through sends the guest's node every
// reply of the contract, so the guest's apps hear what the partners' apps
// hear.

// guestTimeout bounds a request from a guest's node to a host.
var guestTimeout = 30 * time.Second

// ErrNotGuest is returned for a guest's operation on a contract the agent is
// not a guest of.
var ErrNotGuest = errors.New("not a guest of this contract")

// pendingJoin is a guest's join sent to a host, waiting for its welcome.
type pendingJoin struct {
	TxID string            `json:"txId"`
	Host ethcommon.Address `json:"host"`
}

func (r *Router) pendingKey(agent ethcommon.Address, contract string) []byte {
	k := append([]byte("ibc/gj/"), agent.Bytes()...)
	k = binary.AppendUvarint(k, uint64(len(contract)))
	return append(k, contract...)
}

func (r *Router) savePending(agent ethcommon.Address, contract string, p pendingJoin) error {
	raw, _ := json.Marshal(p)
	return r.cfg.DB.Put(r.pendingKey(agent, contract), raw)
}

func (r *Router) pending(agent ethcommon.Address, contract string) (*pendingJoin, error) {
	k := r.pendingKey(agent, contract)
	if ok, err := r.cfg.DB.Has(k); err != nil || !ok {
		return nil, err
	}
	raw, err := r.cfg.DB.Get(k)
	if err != nil {
		return nil, err
	}
	var p pendingJoin
	return &p, json.Unmarshal(raw, &p)
}

func (r *Router) deletePending(agent ethcommon.Address, contract string) error {
	return r.cfg.DB.Delete(r.pendingKey(agent, contract))
}

// hostsOf returns the members of a configuration as a guest's hosts.
func hostsOf(cfg *cordial.Config) []execution.Host {
	out := make([]execution.Host, len(cfg.Members))
	for i, m := range cfg.Members {
		out[i] = execution.Host{Agent: m.Agent, NodeKey: m.NodeKey, Node: m.Node}
	}
	return out
}

// toHost sends a guest's message to its hosts: the host it joined through
// first, then the others, until one answers. A host's refusal is final.
func (r *Router) toHost(ctx context.Context, contract string, e execution.GuestEntry, msg map[string]any) ([]byte, error) {
	hosts := slices.Clone(e.Hosts)
	slices.SortStableFunc(hosts, func(a, b execution.Host) int {
		switch {
		case a.Agent == e.Host:
			return -1
		case b.Agent == e.Host:
			return 1
		}
		return 0
	})
	var errs []error
	for _, h := range hosts {
		body, err := r.cfg.Sender.Send(ctx, request(h.Node, h.Agent, contract, msg))
		if err != nil {
			errs = append(errs, fmt.Errorf("host %s: %w", h.Node, err))
			continue
		}
		if m := replyMessage(body); m != "" {
			return nil, errors.New(m)
		}
		return body, nil
	}
	if len(errs) == 0 {
		return nil, errors.New("the contract has no hosts")
	}
	return nil, fmt.Errorf("no host answered: %w", errors.Join(errs...))
}

// guestSubmit sends a guest's transaction to a host.
func (r *Router) guestSubmit(agent ethcommon.Address, contract string, e execution.GuestEntry, stx identity.SignedTx, id ethcommon.Hash) (string, error) {
	switch stx.Tx.Kind {
	case identity.KindCall:
	case identity.KindLeave:
		body, err := stx.Tx.ParseBody()
		if err != nil {
			return "", err
		}
		if body.(*identity.LeaveBody).Member != agent {
			return "", errors.New("a guest only removes itself")
		}
	case identity.KindJoin:
		return "", fmt.Errorf("%s is already a guest of this contract", agent.Hex())
	default:
		return "", fmt.Errorf("a guest does not %s: leave, and join again", stx.Tx.Kind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), guestTimeout)
	defer cancel()
	if _, err := r.toHost(ctx, contract, e, map[string]any{"type": "tx", "tx": stx}); err != nil {
		return "", err
	}
	if stx.Tx.Kind == identity.KindLeave {
		// The guest's node forgets the contract at once; a guest's leave is
		// always approved.
		if err := r.cfg.Execution.RemoveGuest(agent.Hex(), contract); err != nil {
			return "", err
		}
		r.cfg.Publisher.Publish(agent.Hex(), execution.Reply{Request: id.Hex(), Contract: &contract,
			Action: execution.ActionLeave, Reply: true})
	}
	return id.Hex(), nil
}

// Read runs a read for a guest: a host runs it on its copy of the
// contract, as the guest. A contract's refusal (a revert) comes back as the
// value, its message, as for a partner's read.
func (r *Router) Read(ctx context.Context, agent, contract, method string, values map[string]any) (any, error) {
	addr, err := parseAgent(agent)
	if err != nil {
		return nil, err
	}
	e, ok, err := r.cfg.Execution.Guest(agent, contract)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotGuest
	}
	body, err := r.toHost(ctx, contract, e, map[string]any{"type": "read", "guest": addr, "method": method, "values": values})
	if err != nil {
		return nil, err
	}
	var reply struct {
		Value any    `json:"value"`
		Error string `json:"error"`
	}
	if err := execution.DecodeJSON(body, &reply); err != nil {
		return nil, fmt.Errorf("host's read reply: %w", err)
	}
	if reply.Error != "" {
		return reply.Error, nil
	}
	return reply.Value, nil
}

// hostRead serves a guest's read on a host: the guest's node must have sent
// it, and it runs as the guest.
func (r *Router) hostRead(agent ethcommon.Address, contract string, from ethcommon.Address, msg wireMessage) (any, error) {
	if msg.Guest == nil || msg.Method == "" {
		return nil, errors.New("read message needs guest and method")
	}
	if err := r.checkGuest(agent, contract, *msg.Guest, from); err != nil {
		return nil, err
	}
	res, err := r.cfg.Execution.Read(agent.Hex(), msg.Guest.Hex(), execution.Record{Contract: contract,
		Method: msg.Method, Message: execution.Message{Values: msg.Values}})
	if err != nil {
		var ce *common.ContractError
		if errors.As(err, &ce) || errors.Is(err, common.ErrNotImplemented) {
			return map[string]any{"error": err.Error()}, nil
		}
		return nil, err
	}
	return map[string]any{"value": res.Value}, nil
}

// checkGuest checks, on a host, that guest is a guest of the contract and
// that from is its node.
func (r *Router) checkGuest(agent ethcommon.Address, contract string, guest, from ethcommon.Address) error {
	if r.Miner(agent, contract) == nil {
		return fmt.Errorf("%w %s", ErrUnknownContract, contract)
	}
	g, ok, err := r.cfg.Execution.GuestOf(agent.Hex(), contract, guest.Hex())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is %w", guest.Hex(), ErrNotGuest)
	}
	if g.NodeKey != from {
		return fmt.Errorf("node %s is not the node of guest %s", from.Hex(), guest.Hex())
	}
	return nil
}

// welcomeGuest tells an approved guest's node that it is a guest: it sends
// the contract's members (its hosts) and the deploy record, signed by the
// contract's creator, from which the guest's node knows the contract.
func (r *Router) welcomeGuest(agent ethcommon.Address, contract string, e *minerEntry, record execution.Record, b *identity.JoinBody) {
	cfg := e.miner.Config()
	self, ok := cfg.Member(agent)
	if !ok {
		return
	}
	deploy, err := r.cfg.Execution.DeployRecord(agent.Hex(), contract)
	if err != nil {
		slog.Error("welcome guest", "contract", contract, "err", err)
		return
	}
	_ = e.outbox.Put(request(b.Node, record.Tx.Tx.Agent, contract, map[string]any{"type": "guest_welcome",
		"config": cfg, "sender": self, "record": deploy, "txId": record.HashCode}))
}

// guestWelcome makes the agent a guest of the contract on this node, after
// its join was approved.
func (r *Router) guestWelcome(agent ethcommon.Address, contract string, msg wireMessage) error {
	p, err := r.pending(agent, contract)
	if err != nil {
		return err
	}
	if p == nil {
		// Another host's welcome, after the first one.
		if _, ok, err := r.cfg.Execution.Guest(agent.Hex(), contract); err != nil || !ok {
			return err
		}
		return nil
	}
	if len(msg.Record) == 0 {
		return errors.New("guest_welcome without the deploy record")
	}
	rec, err := execution.DecodeRecord(msg.Record)
	if err != nil {
		return err
	}
	if rec.Tx == nil || rec.Tx.Tx.Kind != identity.KindDeploy {
		return errors.New("guest_welcome: the record is not a deploy")
	}
	if id, err := rec.Tx.Verify(); err != nil || id.Hex() != contract {
		return errors.New("guest_welcome: the deploy is not this contract's")
	}
	if err := r.cfg.Execution.StoreGuest(agent.Hex(), contract, rec.Message.Fields,
		execution.GuestEntry{Host: p.Host, Hosts: hostsOf(msg.Config)}); err != nil {
		return err
	}
	if err := r.deletePending(agent, contract); err != nil {
		return err
	}
	slog.Info("joined contract as a guest", "contract", contract, "agent", agent.Hex())
	r.cfg.Publisher.Publish(agent.Hex(), execution.Reply{Request: p.TxID, Contract: &contract,
		Action: "join_contract", Reply: true})
	return nil
}

// Published sends a reply of a contract to its guests' nodes. Each guest
// hears from the host it joined through, or, if that host has left, from
// the first member.
func (r *Router) Published(agent string, reply execution.Reply) {
	if reply.Contract == nil {
		return
	}
	contract := *reply.Contract
	addr, err := parseAgent(agent)
	if err != nil {
		return
	}
	r.mu.Lock()
	e := r.miners[minerKey{addr, contract}]
	r.mu.Unlock()
	if e == nil {
		return
	}
	guests, err := r.cfg.Execution.Guests(agent, contract)
	if err != nil || len(guests) == 0 {
		return
	}
	cfg := e.miner.Config()
	if len(cfg.Members) == 0 {
		return
	}
	for _, g := range guests {
		host := g.Host
		if !cfg.IsMember(host) {
			host = cfg.Members[0].Agent
		}
		if host != addr {
			continue
		}
		_ = e.outbox.Put(request(g.Node, g.Agent, contract, map[string]any{"type": "event", "reply": reply, "config": cfg}))
	}
}

// guestEvent passes a host's reply to the guest's clients, and keeps the
// guest's list of hosts up to date.
func (r *Router) guestEvent(agent ethcommon.Address, contract string, from ethcommon.Address, msg wireMessage) error {
	e, ok, err := r.cfg.Execution.Guest(agent.Hex(), contract)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotGuest
	}
	if !slices.ContainsFunc(e.Hosts, func(h execution.Host) bool { return h.NodeKey == from }) {
		return fmt.Errorf("node %s is not a host of contract %s", from.Hex(), contract)
	}
	if msg.Reply == nil {
		return errors.New("event without a reply")
	}
	if msg.Config != nil && len(msg.Config.Members) > 0 {
		if err := r.cfg.Execution.SetHosts(agent.Hex(), contract, hostsOf(msg.Config)); err != nil {
			return err
		}
	}
	reply := *msg.Reply
	reply.Contract = &contract
	r.cfg.Publisher.Publish(agent.Hex(), reply)
	return nil
}
