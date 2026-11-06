package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/ouripoupko/ibc-go/identity"
)

// queryWindow is how far a query's time may be from the node's clock. A
// query is also accepted only once, so a copied query cannot be sent again.
var queryWindow = 60 * time.Second

// maxSession is the longest a session may last, from now: a leaked session
// key reads for at most this long.
var maxSession = 7 * 24 * time.Hour

var errNoQuery = errors.New("this call needs a signed query")

// guard checks signed queries and remembers the ones it accepted until they
// expire.
type guard struct {
	nodeKey ethcommon.Address // sessions must be for this node
	mu      sync.Mutex
	seen    map[ethcommon.Hash]time.Time
}

func newGuard(nodeKey ethcommon.Address) *guard {
	return &guard{nodeKey: nodeKey, seen: map[ethcommon.Hash]time.Time{}}
}

// check verifies a signed query for action on target's copy of contract, and
// returns who signed it.
func (g *guard) check(sq identity.SignedQuery, action, target, contract string) (ethcommon.Address, error) {
	id, err := sq.Verify()
	if err != nil {
		return ethcommon.Address{}, fmt.Errorf("query: %w", err)
	}
	now := time.Now()
	if sq.Session != nil {
		s := sq.Session.Session
		if s.NodeKey != g.nodeKey {
			return ethcommon.Address{}, fmt.Errorf("session for node key %s, this node's key is %s", s.NodeKey.Hex(), g.nodeKey.Hex())
		}
		expires := time.Unix(int64(s.Expires), 0)
		if !now.Before(expires) {
			return ethcommon.Address{}, errors.New("session expired")
		}
		if expires.Sub(now) > maxSession {
			return ethcommon.Address{}, fmt.Errorf("session lasts longer than %s", maxSession)
		}
	}
	q := sq.Query
	if q.Action != action {
		return ethcommon.Address{}, fmt.Errorf("query for %s sent to %s", q.Action, action)
	}
	if canonicalAgent(q.Target) != canonicalAgent(target) {
		return ethcommon.Address{}, fmt.Errorf("query for agent %s sent to %s", q.Target, target)
	}
	if !strings.EqualFold(q.Contract, contract) {
		return ethcommon.Address{}, fmt.Errorf("query for contract %q sent to %q", q.Contract, contract)
	}
	if d := now.Sub(time.Unix(int64(q.Time), 0)); d > queryWindow || d < -queryWindow {
		return ethcommon.Address{}, errors.New("query expired, or its time is wrong: check the clock")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for h, t := range g.seen {
		if now.Sub(t) > 2*queryWindow {
			delete(g.seen, h)
		}
	}
	if _, ok := g.seen[id]; ok {
		return ethcommon.Address{}, errors.New("query already used")
	}
	g.seen[id] = now
	return q.Agent, nil
}

// owner checks a query that only the target agent itself may make.
func (g *guard) owner(sq *identity.SignedQuery, action, target, contract string) error {
	if sq == nil {
		return errNoQuery
	}
	signer, err := g.check(*sq, action, target, contract)
	if err != nil {
		return err
	}
	if signer.Hex() != canonicalAgent(target) {
		return fmt.Errorf("only agent %s may ask this", canonicalAgent(target))
	}
	return nil
}

// parseQuery decodes a signed query from JSON; empty gives nil.
func parseQuery(raw []byte) (*identity.SignedQuery, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var sq identity.SignedQuery
	if err := json.Unmarshal(raw, &sq); err != nil {
		return nil, fmt.Errorf("signed query: %w", err)
	}
	if len(sq.Signature) == 0 {
		return nil, nil // not a signed query, e.g. an old client's body
	}
	return &sq, nil
}

// canonicalAgent writes an agent id that is an address in its checksummed form.
func canonicalAgent(agent string) string {
	if ethcommon.IsHexAddress(agent) {
		return ethcommon.HexToAddress(agent).Hex()
	}
	return agent
}
