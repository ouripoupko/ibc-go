package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/identity"
)

// AppsCollection is where a node keeps the apps its agents connected.
const AppsCollection = "apps_by_site"

// permission is what an agent lets an app do with contracts of one code.
type permission struct {
	Read      bool                  `json:"read"`      // read without asking
	Functions []string              `json:"functions"` // have the node sign these calls without asking
	Grant     *identity.Certificate `json:"grant,omitempty"`
}

// any reports whether the permission allows anything.
func (p permission) any() bool { return p.Read || len(p.Functions) > 0 }

// appRecord is an app an agent connected: its site (origin), the keys of the
// browsers it runs in (its devices), and its permissions by contract code
// hash. The zero code hash means every contract: the node's own web page.
type appRecord struct {
	Agent       string                `json:"agent"`
	App         string                `json:"app"`
	Keys        []string              `json:"keys"`
	Permissions map[string]permission `json:"permissions"`
}

var everyContract = ethcommon.Hash{}.Hex()

// owner reports whether the app is the node's own page, which may see
// everything of the agent.
func (r *appRecord) owner() bool {
	_, ok := r.Permissions[everyContract]
	return ok
}

// permission returns what the app may do with contracts of codeHash.
func (r *appRecord) permission(codeHash ethcommon.Hash) permission {
	if p, ok := r.Permissions[everyContract]; ok {
		return p
	}
	return r.Permissions[codeHash.Hex()]
}

// apps keeps appRecords: a document per (agent, site), and an index from
// each device key to its site.
type apps struct {
	mu   sync.Mutex
	coll common.Collection
}

func siteID(agent, app string) string { return "site " + agent + " " + app }
func keyID(agent, key string) string  { return "key " + agent + " " + strings.ToLower(key) }

func (a *apps) read(id string, out any) (bool, error) {
	fields, err := a.coll.Document(id).Fields()
	if err != nil {
		return false, err
	}
	raw, _ := fields["value"].(string)
	if raw == "" {
		return false, nil
	}
	return true, json.Unmarshal([]byte(raw), out)
}

func (a *apps) write(id string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return a.coll.Document(id).Update(map[string]any{"value": string(raw)})
}

func (a *apps) site(agent, app string) (*appRecord, error) {
	var r appRecord
	ok, err := a.read(siteID(agent, app), &r)
	if err != nil || !ok {
		return nil, err
	}
	return &r, nil
}

// get returns the app whose device key this is, or nil.
func (a *apps) get(agent, key string) (*appRecord, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var app string
	ok, err := a.read(keyID(agent, key), &app)
	if err != nil || !ok {
		return nil, err
	}
	r, err := a.site(agent, app)
	if err != nil || r == nil || !slices.Contains(r.Keys, strings.ToLower(key)) {
		return nil, err
	}
	return r, nil
}

// addKeyLocked adds a device key to an app, if no other app has it.
func (a *apps) addKeyLocked(r *appRecord, key string) error {
	key = strings.ToLower(key)
	var owner string
	if ok, err := a.read(keyID(r.Agent, key), &owner); err != nil {
		return err
	} else if ok && owner != r.App {
		return fmt.Errorf("this key belongs to %s, not %s", owner, r.App)
	}
	if !slices.Contains(r.Keys, key) {
		r.Keys = append(r.Keys, key)
	}
	return a.write(keyID(r.Agent, key), r.App)
}

// consent records a consent the agent's root signed, with its grant if it
// names functions: it replaces the app's permission for that code, and
// connects the consent's key as a device of the app.
func (a *apps) consent(c identity.Consent, grant *identity.Certificate) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	agent := c.Agent.Hex()
	r, err := a.site(agent, c.App)
	if err != nil {
		return err
	}
	if r == nil {
		r = &appRecord{Agent: agent, App: c.App, Keys: []string{}, Permissions: map[string]permission{}}
	}
	if err := a.addKeyLocked(r, hexutil.Encode(c.AppKey)); err != nil {
		return err
	}
	r.Permissions[c.CodeHash.Hex()] = permission{Read: c.Read, Functions: slices.Clone(c.Functions), Grant: grant}
	return a.write(siteID(agent, c.App), r)
}

// device connects another browser of an app the agent approved before.
func (a *apps) device(d identity.Device) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	agent := d.Agent.Hex()
	r, err := a.site(agent, d.App)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("agent %s has not approved %s yet", agent, d.App)
	}
	if err := a.addKeyLocked(r, hexutil.Encode(d.AppKey)); err != nil {
		return err
	}
	return a.write(siteID(agent, d.App), r)
}

// remove disconnects an app: all its devices and permissions.
func (a *apps) remove(agent, app string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.site(agent, app)
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("no such app")
	}
	for _, k := range r.Keys {
		if err := a.coll.Delete(keyID(agent, k)); err != nil {
			return err
		}
	}
	return a.coll.Delete(siteID(agent, app))
}

// list returns the agent's apps, by site.
func (a *apps) list(agent string) ([]appRecord, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids, err := a.coll.Keys()
	if err != nil {
		return nil, err
	}
	out := []appRecord{}
	prefix := siteID(agent, "")
	for _, id := range ids {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		r, err := a.site(agent, strings.TrimPrefix(id, prefix))
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out, nil
}

// sameFunctions reports whether two lists name the same functions.
func sameFunctions(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
