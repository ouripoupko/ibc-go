package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// queryWindow is how far a query's time may be from the node's clock. A
// query is also accepted only once, so a copied query cannot be sent again.
var queryWindow = 60 * time.Second

var errNoQuery = errors.New("this call needs a signed query")

// errNeedsApproval means the app may not do this without the user: it should
// send the user to the node's approval page.
var errNeedsApproval = errors.New("needs your approval on your node's page")

// guard checks signed queries and remembers the ones it accepted until they
// expire.
type guard struct {
	mu   sync.Mutex
	seen map[ethcommon.Hash]time.Time
}

func newGuard() *guard {
	return &guard{seen: map[ethcommon.Hash]time.Time{}}
}

// check verifies a signed query for action on target's copy of contract, and
// returns the key that signed it. A query acts for the agent it names, which
// must be the target: an agent reads only its own copy.
func (g *guard) check(sq identity.SignedQuery, action, target, contract string) (identity.Signer, error) {
	now := time.Now()
	id, signer, err := sq.Verify()
	if err != nil {
		return identity.Signer{}, fmt.Errorf("query: %w", err)
	}
	q := sq.Query
	if q.Action != action {
		return identity.Signer{}, fmt.Errorf("query for %s sent to %s", q.Action, action)
	}
	if canonicalAgent(q.Target) != canonicalAgent(target) || q.Agent.Hex() != canonicalAgent(target) {
		return identity.Signer{}, fmt.Errorf("query for agent %s sent to %s", q.Target, target)
	}
	if !strings.EqualFold(q.Contract, contract) {
		return identity.Signer{}, fmt.Errorf("query for contract %q sent to %q", q.Contract, contract)
	}
	if d := now.Sub(time.Unix(int64(q.Time), 0)); d > queryWindow || d < -queryWindow {
		return identity.Signer{}, errors.New("query expired, or its time is wrong: check the clock")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for h, t := range g.seen {
		if now.Sub(t) > 2*queryWindow {
			delete(g.seen, h)
		}
	}
	if _, ok := g.seen[id]; ok {
		return identity.Signer{}, errors.New("query already used")
	}
	g.seen[id] = now
	return signer, nil
}

// caller is who made a query: the agent itself, with its root key, or an app
// the agent connected.
type caller struct {
	agent string
	app   *appRecord // nil for the root key
	key   string     // the app's device key, hex
}

// owner reports whether the caller may see everything of the agent: its
// root key, or the node's own page.
func (c caller) owner() bool { return c.app == nil || c.app.owner() }

// authorize checks a signed query and finds who made it.
func (d *Dispatcher) authorize(sq *identity.SignedQuery, action, target, contract string) (caller, error) {
	if sq == nil {
		return caller{}, errNoQuery
	}
	signer, err := d.guard.check(*sq, action, target, contract)
	if err != nil {
		return caller{}, err
	}
	agent := canonicalAgent(target)
	if signer.Agent.Hex() == agent {
		return caller{agent: agent}, nil
	}
	key := hexutil.Encode(signer.Key)
	app, err := d.apps.get(agent, key)
	if err != nil {
		return caller{}, err
	}
	if app == nil {
		return caller{}, fmt.Errorf("this key is not agent %s's, nor an app it connected: connect the app on the node's page", agent)
	}
	return caller{agent: agent, app: app, key: key}, nil
}

// sourceHash returns the source hash of the agent's contract.
func (d *Dispatcher) sourceHash(agent, contract string) (ethcommon.Hash, error) {
	summary, ok, err := d.navigator.Contract(agent, contract)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	if !ok {
		return ethcommon.Hash{}, fmt.Errorf("%w %q", execution.ErrUnknownContract, contract)
	}
	source, _ := summary[execution.KeySource].(string)
	if source == "" {
		return ethcommon.Hash{}, fmt.Errorf("contract %s has no source", contract)
	}
	return identity.SourceHash(source), nil
}

// canHear checks that the caller may hear the contract's events: it has a
// permission for the contract's source (to read, or to call silently).
func (d *Dispatcher) canHear(c caller, contract string) error {
	if c.owner() {
		return nil
	}
	hash, err := d.sourceHash(c.agent, contract)
	if err != nil {
		return err
	}
	if !c.app.permission(hash).any() {
		return fmt.Errorf("this contract's events %w", errNeedsApproval)
	}
	return nil
}

// canRead checks that the caller may read the contract without asking.
func (d *Dispatcher) canRead(c caller, contract string) error {
	if c.owner() {
		return nil
	}
	hash, err := d.sourceHash(c.agent, contract)
	if err != nil {
		return err
	}
	if !c.app.permission(hash).Read {
		return fmt.Errorf("reading this contract %w", errNeedsApproval)
	}
	return nil
}

// parseQuery decodes a signed query from JSON; empty gives nil.
func parseQuery(raw []byte) (*identity.SignedQuery, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("signed query: %w", err)
	}
	if _, ok := fields["query"]; !ok {
		return nil, nil // not a query (a signed transaction, for example)
	}
	var sq identity.SignedQuery
	if err := json.Unmarshal(raw, &sq); err != nil {
		return nil, fmt.Errorf("signed query: %w", err)
	}
	if sq.Signature.Scheme == "" && len(sq.Signature.Sig) == 0 {
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
