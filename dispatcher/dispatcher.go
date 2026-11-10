// Package dispatcher is the node's HTTP API (/ibc/app and the /stream
// event stream), described in docs/api.md.
package dispatcher

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/compiler"
	"github.com/ouripoupko/ibc-go/consensus"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// Actions handled by the dispatcher itself.
const (
	actionNodeInfo      = "node_info"
	actionIsExistAgent  = "is_exist_agent"
	actionAgentInfo     = "agent_info"
	actionGetContracts  = "get_contracts"
	actionGetContract   = "get_contract"
	actionJoinContract  = "join_contract"
	actionLeaveContract = "leave_contract"
	actionRotateKey     = "rotate_key"
	actionRead          = "contract_read"
	actionStream        = "stream"
	actionRegisterApp   = "register_app"
	actionGetApps       = "get_apps"
	actionRevokeApp     = "revoke_app"
	actionCompile       = "compile"
)

// maxBody bounds a request body.
const maxBody = 16 << 20

// keepAlive is the interval of the empty event stream messages.
var keepAlive = 10 * time.Second

// Dispatcher serves the HTTP API.
type Dispatcher struct {
	navigator *execution.Navigator
	consensus consensus.Protocol
	broker    *Broker
	key       *ecdsa.PrivateKey // the node's key: signs calls silently, under grants
	nodeKey   ethcommon.Address
	storage   func() (int64, bool)
	compiler  *compiler.Solc
	apps      *apps
	done      <-chan struct{}
	mux       *http.ServeMux
	guard     *guard
}

// Options are a Dispatcher's dependencies besides the node's parts.
type Options struct {
	// NodeKey is the node's key, which agents authorize when they register,
	// and with which the node signs calls silently, under their grants.
	NodeKey *ecdsa.PrivateKey
	// Storage reports the size of the node's data folder, if it has one.
	Storage func() (int64, bool)
	// Apps keeps the apps agents connected (AppsCollection).
	Apps common.Collection
	// Compiler compiles Solidity source; nil: the node cannot compile.
	Compiler *compiler.Solc
}

// New returns a Dispatcher. Event streams end when done is closed.
func New(navigator *execution.Navigator, protocol consensus.Protocol, broker *Broker, opts Options,
	done <-chan struct{}) *Dispatcher {
	storage := opts.Storage
	if storage == nil {
		storage = func() (int64, bool) { return 0, false }
	}
	d := &Dispatcher{navigator: navigator, consensus: protocol, broker: broker, key: opts.NodeKey,
		nodeKey: identity.Address(opts.NodeKey), storage: storage, compiler: opts.Compiler,
		apps: &apps{coll: opts.Apps}, done: done, guard: newGuard()}
	d.mux = http.NewServeMux()
	d.mux.HandleFunc("/ibc/app", d.handleApp)
	d.mux.HandleFunc("/ibc/app/{path...}", d.handleApp)
	d.mux.HandleFunc("GET /stream", d.handleStream)
	return d
}

// ServeHTTP adds the CORS headers and answers preflight requests.
func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	d.mux.ServeHTTP(w, r)
}

// handleApp serves /ibc/app/<agent>/<contract>/<method>?action=<action>.
func (d *Dispatcher) handleApp(w http.ResponseWriter, r *http.Request) {
	var parts [3]string
	segments := strings.Split(r.PathValue("path"), "/")
	if len(segments) > 3 {
		writeJSON(w, http.StatusNotFound, message("not found"))
		return
	}
	copy(parts[:], segments)
	// An agent id that is an address is written in its checksummed form.
	parts[0] = canonicalAgent(parts[0])

	body, msg, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, message(err.Error()))
		return
	}
	record := execution.Record{
		Type:      r.Method,
		Action:    r.URL.Query().Get("action"),
		Agent:     parts[0],
		Contract:  parts[1],
		Method:    parts[2],
		Message:   msg,
		Timestamp: Timestamp(time.Now()),
	}
	record.HashCode = hashRecord(record)

	if record.Action != execution.ActionConsent {
		slog.Info("request", "hash", short(record.HashCode), "agent", record.Agent,
			"action", record.Action, "contract", short(record.Contract), "method", record.Method)
	}
	reply, err := d.handle(r.Context(), record, body, r.URL.Query())
	if err != nil {
		if record.Action != execution.ActionConsent {
			slog.Warn("request failed", "hash", short(record.HashCode), "action", record.Action, "err", err)
		}
		reply = message(err.Error())
	}
	writeJSON(w, http.StatusOK, reply)
}

// handle runs an action. query is the URL's parameters: for a GET, the
// signed query is its "auth".
// Errors in the request itself come back as JSON values with status 200,
// not as HTTP errors.
func (d *Dispatcher) handle(ctx context.Context, record execution.Record, body []byte, query url.Values) (any, error) {
	auth := query.Get("auth")
	type route struct{ method, action string }
	switch (route{record.Type, record.Action}) {
	case route{http.MethodGet, actionNodeInfo}:
		info := map[string]any{"nodeKey": d.nodeKey.Hex()}
		if size, ok := d.storage(); ok {
			info["storage"] = size
		}
		return info, nil

	case route{http.MethodGet, actionIsExistAgent}:
		return d.navigator.AgentExists(record.Agent)

	case route{http.MethodGet, actionAgentInfo}:
		agent := record.Agent
		if credential := query.Get("credential"); agent == "" && credential != "" {
			found, ok, err := d.navigator.AgentByCredential(credential)
			if err != nil || !ok {
				return message("no agent with this passkey on this node"), err
			}
			agent = found
		}
		info, ok, err := d.navigator.AgentInfo(agent)
		if err != nil || !ok {
			return message("no such agent on this node"), err
		}
		return info, nil

	case route{http.MethodGet, actionGetContracts}:
		c, err := d.authorizeURL(auth, record, actionGetContracts)
		if err != nil {
			return nil, err
		}
		list, err := d.navigator.Contracts(record.Agent)
		if err != nil || c.owner() {
			return list, err
		}
		// An app sees the contracts it may read.
		mine := []map[string]any{}
		for _, summary := range list {
			code, _ := summary[common.KeyCode].(string)
			if hash, err := identity.CodeHash(code); err == nil && c.app.permission(hash).Read {
				mine = append(mine, summary)
			}
		}
		return mine, nil

	case route{http.MethodGet, actionGetContract}:
		c, err := d.authorizeURL(auth, record, actionGetContract)
		if err != nil {
			return nil, err
		}
		if err := d.canRead(c, record.Contract); err != nil {
			if errors.Is(err, execution.ErrUnknownContract) {
				return map[string]any{}, nil
			}
			return nil, err
		}
		summary, ok, err := d.navigator.Contract(record.Agent, record.Contract)
		if err != nil || !ok {
			return map[string]any{}, err
		}
		return summary, nil

	case route{http.MethodPut, execution.ActionRegisterAgent}:
		return d.registerAgent(record, body)

	case route{http.MethodPut, execution.ActionDeployContract}:
		return d.submit(record, body, identity.KindDeploy)

	case route{http.MethodPost, execution.ActionWrite}:
		if sq, err := parseQuery(body); err != nil || sq != nil {
			if err != nil {
				return nil, err
			}
			return d.silentWrite(record, sq)
		}
		return d.submit(record, body, identity.KindCall)

	case route{http.MethodPut, actionRegisterApp}:
		return d.registerApp(record, body)

	case route{http.MethodGet, actionGetApps}:
		c, err := d.authorizeURL(auth, record, actionGetApps)
		if err != nil {
			return nil, err
		}
		if !c.owner() {
			return nil, fmt.Errorf("only agent %s may ask this", record.Agent)
		}
		return d.apps.list(record.Agent)

	case route{http.MethodPut, actionRevokeApp}:
		sq, err := parseQuery(body)
		if err != nil {
			return nil, err
		}
		c, err := d.authorize(sq, actionRevokeApp, record.Agent, "")
		if err != nil {
			return nil, err
		}
		if c.app != nil {
			return nil, errors.New("revoking an app needs the root key")
		}
		var req struct {
			App string `json:"app"`
		}
		if err := json.Unmarshal([]byte(sq.Query.Body), &req); err != nil || req.App == "" {
			return nil, errors.New("revoke_app body needs app (the app's site)")
		}
		return true, d.apps.remove(record.Agent, req.App)

	case route{http.MethodPost, actionCompile}:
		var req struct {
			Source   string `json:"source"`
			Contract string `json:"contract"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Source == "" {
			return nil, errors.New("compile needs {source, contract}")
		}
		res, err := d.compiler.Compile(ctx, req.Source, req.Contract)
		if err != nil {
			return nil, err
		}
		return res, nil

	case route{http.MethodPut, actionLeaveContract}:
		return d.submit(record, body, identity.KindLeave)

	case route{http.MethodPut, actionRotateKey}:
		return d.submit(record, body, identity.KindRotateKey)

	case route{http.MethodPut, actionJoinContract}:
		var req struct {
			identity.SignedTx
			Partner struct {
				Address string `json:"address"`
				Agent   string `json:"agent"`
			} `json:"partner"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("join_contract: %w", err)
		}
		if req.Partner.Address == "" || req.Partner.Agent == "" {
			return nil, errors.New("join_contract needs partner.address and partner.agent")
		}
		if have, err := d.navigator.ContractExists(record.Agent, req.Tx.Contract); err != nil || have {
			return nil, err // null: this agent already has the contract
		}
		return d.consensus.RelayJoin(ctx, record.Agent, req.SignedTx, req.Partner.Address, req.Partner.Agent)

	case route{http.MethodPut, execution.ActionConsent}:
		return d.consensus.Receive(ctx, record.Agent, record.Contract, body)

	case route{http.MethodPost, actionRead}:
		if err := d.readQuery(&record, body); err != nil {
			if errors.Is(err, execution.ErrUnknownContract) {
				return message("no such contract"), nil
			}
			return nil, err
		}
		// The read runs as the agent: msg.sender is the agent itself.
		result, err := d.navigator.Read(record.Agent, record.Agent, record)
		if errors.Is(err, execution.ErrUnknownContract) {
			return message("no such contract"), nil
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
	return nil, fmt.Errorf("unknown action %q for %s", record.Action, record.Type)
}

// authorizeURL checks the signed query of a GET, from its auth parameter.
func (d *Dispatcher) authorizeURL(auth string, record execution.Record, action string) (caller, error) {
	sq, err := parseQuery([]byte(auth))
	if err != nil {
		return caller{}, err
	}
	return d.authorize(sq, action, record.Agent, record.Contract)
}

// callOf decodes the {method, values} body of a read or a silent write.
func callOf(sq *identity.SignedQuery, method string) (identity.CallBody, error) {
	var call identity.CallBody
	if err := execution.DecodeJSON([]byte(sq.Query.Body), &call); err != nil {
		return call, fmt.Errorf("%s body: %w", sq.Query.Action, err)
	}
	if call.Method == "" {
		return call, fmt.Errorf("%s body needs a method", sq.Query.Action)
	}
	if method != "" && method != call.Method {
		return call, fmt.Errorf("query calls %s, sent to %s", call.Method, method)
	}
	return call, nil
}

// readQuery checks a contract_read's signed query, and whether its signer may
// read the contract without asking, and puts the call it signs into record.
func (d *Dispatcher) readQuery(record *execution.Record, body []byte) error {
	sq, err := parseQuery(body)
	if err != nil {
		return err
	}
	c, err := d.authorize(sq, actionRead, record.Agent, record.Contract)
	if err != nil {
		return err
	}
	call, err := callOf(sq, record.Method)
	if err != nil {
		return err
	}
	if err := d.canRead(c, record.Contract); err != nil {
		return err
	}
	record.Method = call.Method
	record.Message = execution.Message{Values: call.Values}
	return nil
}

// silentWrite serves a call an app asks the node to sign without the user:
// if the user's consent lets the app have this function called silently, the
// node signs the call with its key, under the user's grant, and sends it to
// consensus. Otherwise the app must send the user to the node's page.
func (d *Dispatcher) silentWrite(record execution.Record, sq *identity.SignedQuery) (any, error) {
	c, err := d.authorize(sq, execution.ActionWrite, record.Agent, record.Contract)
	if err != nil {
		return nil, err
	}
	call, err := callOf(sq, record.Method)
	if err != nil {
		return nil, err
	}
	if c.app == nil {
		return nil, errors.New("the root key signs its own transactions: send a signed transaction")
	}
	hash, err := d.codeHash(c.agent, record.Contract)
	if err != nil {
		return nil, err
	}
	p := c.app.permission(hash)
	if p.Grant == nil || !p.Grant.Grant.Allows(call.Method) || !slices.Contains(p.Functions, call.Method) {
		return nil, fmt.Errorf("calling %s %w", call.Method, errNeedsApproval)
	}
	raw, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	stx, err := identity.SignTx(identity.Transaction{Contract: record.Contract, Kind: identity.KindCall,
		Agent: ethcommon.HexToAddress(c.agent), Nonce: sq.Query.Nonce, Body: string(raw)},
		identity.Secp256k1Signer{Key: d.key, Cert: p.Grant})
	if err != nil {
		return nil, err
	}
	return d.consensus.SubmitTx(record.Agent, stx)
}

// registerApp records what the agent lets an app do: a consent signed by the
// agent's root key, with the grant for the node if it names functions; or a
// device: another browser of an app the agent approved, with the same
// permissions.
func (d *Dispatcher) registerApp(record execution.Record, body []byte) (any, error) {
	var req struct {
		Consent *identity.SignedConsent `json:"consent"`
		Grant   *identity.Certificate   `json:"grant"`
		Device  *identity.SignedDevice  `json:"device"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("register_app: %w", err)
	}
	if req.Device != nil {
		// Another browser of an app the agent approved: same permissions.
		if err := req.Device.Verify(); err != nil {
			return nil, fmt.Errorf("register_app: %w", err)
		}
		if req.Device.Device.Agent.Hex() != record.Agent {
			return nil, fmt.Errorf("device of %s sent as %s", req.Device.Device.Agent.Hex(), record.Agent)
		}
		return true, d.apps.device(req.Device.Device)
	}
	if req.Consent == nil {
		return nil, errors.New("register_app needs a consent or a device")
	}
	if err := req.Consent.Verify(); err != nil {
		return nil, fmt.Errorf("register_app: %w", err)
	}
	c := req.Consent.Consent
	if c.Agent.Hex() != record.Agent {
		return nil, fmt.Errorf("consent of %s sent as %s", c.Agent.Hex(), record.Agent)
	}
	if registered, err := d.navigator.AgentExists(record.Agent); err != nil || !registered {
		return nil, fmt.Errorf("agent %s is not registered here", record.Agent)
	}
	if c.CodeHash == (ethcommon.Hash{}) && len(c.Functions) > 0 {
		return nil, errors.New("a consent for every contract only lets the app read")
	}
	if len(c.Functions) > 0 {
		g := req.Grant
		if g == nil {
			return nil, errors.New("register_app: functions to call silently need the grant for this node")
		}
		if err := identity.VerifyGrant(g); err != nil {
			return nil, fmt.Errorf("register_app: %w", err)
		}
		if g.Grant.Agent != c.Agent || g.Grant.NodeKey != d.nodeKey || g.Grant.CodeHash != c.CodeHash ||
			!sameFunctions(g.Grant.Functions, c.Functions) {
			return nil, errors.New("register_app: the grant does not match the consent, or names another node")
		}
	} else {
		req.Grant = nil
	}
	if err := d.apps.consent(c, req.Grant); err != nil {
		return nil, err
	}
	return true, nil
}

// registerAgent registers an agent from its signed registration: the agent
// authorizes this node, by its key, to act for it.
func (d *Dispatcher) registerAgent(record execution.Record, body []byte) (any, error) {
	var sr identity.SignedRegistration
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("register_agent: %w", err)
	}
	id, root, err := sr.Verify()
	if err != nil {
		return nil, fmt.Errorf("register_agent: %w", err)
	}
	reg := sr.Registration
	if reg.Agent.Hex() != record.Agent {
		return nil, fmt.Errorf("registration of %s sent as %s", reg.Agent.Hex(), record.Agent)
	}
	if reg.NodeKey != d.nodeKey {
		return nil, fmt.Errorf("registration authorizes node key %s, this node's key is %s", reg.NodeKey.Hex(), d.nodeKey.Hex())
	}
	record.HashCode = id.Hex()
	record.Message.Fields = map[string]any{"address": reg.Node, "nodeKey": reg.NodeKey.Hex(),
		"rootKey": hexutil.Encode(root.Key), "rootScheme": sr.Signature.Scheme, "credential": hexutil.Encode(reg.Credential)}
	if err := d.navigator.Submit(record.Agent, record); err != nil {
		return nil, err
	}
	return id.Hex(), nil
}

// submit hands a signed transaction of the expected kind to consensus.
func (d *Dispatcher) submit(record execution.Record, body []byte, kind string) (any, error) {
	var stx identity.SignedTx
	if err := json.Unmarshal(body, &stx); err != nil {
		return nil, fmt.Errorf("%s: %w", record.Action, err)
	}
	if stx.Tx.Kind != kind {
		return nil, fmt.Errorf("%s needs a %s transaction, got %q", record.Action, kind, stx.Tx.Kind)
	}
	if kind == identity.KindCall {
		if record.Contract != "" && !strings.EqualFold(record.Contract, stx.Tx.Contract) {
			return nil, fmt.Errorf("transaction for contract %s sent to %s", stx.Tx.Contract, record.Contract)
		}
		if record.Method != "" {
			var call identity.CallBody
			if err := execution.DecodeJSON([]byte(stx.Tx.Body), &call); err == nil && call.Method != record.Method {
				return nil, fmt.Errorf("transaction calls %s, sent to %s", call.Method, record.Method)
			}
		}
	}
	return d.consensus.SubmitTx(record.Agent, stx)
}

// handleStream serves /stream?auth=<signed query>&auth=..., the Server-Sent
// Events stream of replies. Each signed query, with action "stream", opens
// the stream of its agent (its target), for its contract or for all of them.
// The agent's root key may sign it, or an app the agent connected, which then
// hears only about the contracts it has a permission for.
func (d *Dispatcher) handleStream(w http.ResponseWriter, r *http.Request) {
	auths := r.URL.Query()["auth"]
	if len(auths) == 0 {
		writeJSON(w, http.StatusUnauthorized, message(errNoQuery.Error()))
		return
	}
	scopes := make([]Scope, len(auths))
	agents := make([]string, len(auths))
	for i, auth := range auths {
		sq, err := parseQuery([]byte(auth))
		if err == nil && sq == nil {
			err = errNoQuery
		}
		var c caller
		if err == nil {
			c, err = d.authorize(sq, actionStream, sq.Query.Target, sq.Query.Contract)
		}
		if err == nil && !c.owner() && sq.Query.Contract != "" {
			err = d.canHear(c, sq.Query.Contract)
		}
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, message(err.Error()))
			return
		}
		scopes[i] = Scope{Agent: canonicalAgent(sq.Query.Target), Contract: sq.Query.Contract}
		if !c.owner() {
			// An app hears only about the contracts it has a permission for.
			agent := c.agent
			scopes[i].Allow = func(contract string) bool {
				app, err := d.apps.get(agent, c.key)
				if err != nil || app == nil {
					return false
				}
				hash, err := d.codeHash(agent, contract)
				return err == nil && app.permission(hash).any()
			}
		}
		agents[i] = scopes[i].Agent
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	events, cancel := d.broker.Subscribe(scopes)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	slog.Info("stream connected", "agents", agents)
	if !writeEvent(w, flusher, nil) {
		return
	}
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return // too slow: the client reconnects
			}
			if !writeEvent(w, flusher, ev) {
				return
			}
		case <-ticker.C:
			if !writeEvent(w, flusher, nil) {
				return
			}
		case <-r.Context().Done():
			return
		case <-d.done:
			return
		}
	}
}

// writeEvent writes one SSE message; nil writes the empty keep-alive.
func writeEvent(w io.Writer, f http.Flusher, ev any) bool {
	data := []byte{}
	if ev != nil {
		var err error
		if data, err = json.Marshal(ev); err != nil {
			slog.Error("encode event", "err", err)
			return true
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	f.Flush()
	return true
}

// readBody reads a JSON body. Like Flask's get_json, anything that is not
// declared as JSON gives no body and a null message.
func readBody(r *http.Request) ([]byte, execution.Message, error) {
	var msg execution.Message
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		return nil, msg, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, msg, fmt.Errorf("read body: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, msg, nil
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, msg, fmt.Errorf("invalid JSON body: %w", err)
	}
	return body, msg, nil
}

// Timestamp formats t as a record timestamp, YYYYMMDDHHMMSSffffff in UTC.
// (Python used the node's local time; UTC is what common.ParseTimestamp
// assumes, so block.timestamp is the same on every partner.)
func Timestamp(t time.Time) string {
	return strings.Replace(t.UTC().Format("20060102150405.000000"), ".", "", 1)
}

// hashRecord is the record's id: SHA-256 of its JSON encoding without the
// hash_code. Go encodes struct fields in a fixed order and map keys sorted,
// so the encoding, and the hash, are stable. (Python hashed str(record).)
func hashRecord(record execution.Record) string {
	record.HashCode = ""
	raw, err := json.Marshal(record)
	if err != nil {
		raw = []byte(fmt.Sprintf("%+v", record))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func message(text string) map[string]any { return map[string]any{"message": text} }

func short(hash string) string {
	if len(hash) > 10 {
		return hash[:10]
	}
	return hash
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}
