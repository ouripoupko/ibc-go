// Package dispatcher is the node's HTTP API (/ibc/app and the /stream
// event stream), described in docs/api.md.
package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/consensus"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// Actions handled by the dispatcher itself.
const (
	actionNodeInfo      = "node_info"
	actionIsExistAgent  = "is_exist_agent"
	actionGetContracts  = "get_contracts"
	actionGetContract   = "get_contract"
	actionJoinContract  = "join_contract"
	actionLeaveContract = "leave_contract"
	actionRotateKey     = "rotate_key"
	actionRead          = "contract_read"
	actionStream        = "stream"
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
	nodeKey   ethcommon.Address
	done      <-chan struct{}
	mux       *http.ServeMux
	guard     *guard
}

// New returns a Dispatcher. nodeKey is the address of the node's key, which
// agents authorize when they register. Event streams end when done is closed.
func New(navigator *execution.Navigator, protocol consensus.Protocol, broker *Broker,
	nodeKey ethcommon.Address, done <-chan struct{}) *Dispatcher {
	d := &Dispatcher{navigator: navigator, consensus: protocol, broker: broker, nodeKey: nodeKey, done: done,
		guard: newGuard(nodeKey)}
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
	reply, err := d.handle(r.Context(), record, body, r.URL.Query().Get("auth"))
	if err != nil {
		if record.Action != execution.ActionConsent {
			slog.Warn("request failed", "hash", short(record.HashCode), "action", record.Action, "err", err)
		}
		reply = message(err.Error())
	}
	writeJSON(w, http.StatusOK, reply)
}

// handle runs an action. auth is the signed query of a GET, from the URL.
// Errors in the request itself come back as JSON values with status 200,
// not as HTTP errors.
func (d *Dispatcher) handle(ctx context.Context, record execution.Record, body []byte, auth string) (any, error) {
	type route struct{ method, action string }
	switch (route{record.Type, record.Action}) {
	case route{http.MethodGet, actionNodeInfo}:
		return map[string]any{"nodeKey": d.nodeKey.Hex()}, nil

	case route{http.MethodGet, actionIsExistAgent}:
		return d.navigator.AgentExists(record.Agent)

	case route{http.MethodGet, actionGetContracts}:
		if err := d.ownerQuery(auth, record, actionGetContracts); err != nil {
			return nil, err
		}
		return d.navigator.Contracts(record.Agent)

	case route{http.MethodGet, actionGetContract}:
		if err := d.ownerQuery(auth, record, actionGetContract); err != nil {
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
		return d.submit(record, body, identity.KindCall)

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
		caller, err := d.readQuery(&record, body)
		if err != nil {
			return nil, err
		}
		result, err := d.navigator.Read(record.Agent, caller, record)
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

// ownerQuery checks the signed query of a call that only the agent itself
// may make.
func (d *Dispatcher) ownerQuery(auth string, record execution.Record, action string) error {
	sq, err := parseQuery([]byte(auth))
	if err != nil {
		return err
	}
	return d.guard.owner(sq, action, record.Agent, record.Contract)
}

// readQuery checks a contract_read's signed query and puts the call it signs
// into record. It returns the signer, the read's msg.sender.
func (d *Dispatcher) readQuery(record *execution.Record, body []byte) (string, error) {
	sq, err := parseQuery(body)
	if err != nil {
		return "", err
	}
	if sq == nil {
		return "", errNoQuery
	}
	signer, err := d.guard.check(*sq, actionRead, record.Agent, record.Contract)
	if err != nil {
		return "", err
	}
	var call struct {
		Method string         `json:"method"`
		Values map[string]any `json:"values"`
	}
	if err := execution.DecodeJSON([]byte(sq.Query.Body), &call); err != nil {
		return "", fmt.Errorf("contract_read body: %w", err)
	}
	if call.Method == "" {
		return "", errors.New("contract_read body needs a method")
	}
	if record.Method != "" && record.Method != call.Method {
		return "", fmt.Errorf("query reads %s, sent to %s", call.Method, record.Method)
	}
	record.Method = call.Method
	record.Message = execution.Message{Values: call.Values}
	return signer.Hex(), nil
}

// registerAgent registers an agent from its signed registration: the agent
// authorizes this node, by its key, to act for it.
func (d *Dispatcher) registerAgent(record execution.Record, body []byte) (any, error) {
	var sr identity.SignedRegistration
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("register_agent: %w", err)
	}
	id, err := sr.Verify()
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
	record.Message.Fields = map[string]any{"address": reg.Node, "nodeKey": reg.NodeKey.Hex()}
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
// the stream of its agent (its target), for its contract or for all of them;
// only the agent itself may sign it.
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
		if err == nil {
			err = d.guard.owner(sq, actionStream, sq.Query.Target, sq.Query.Contract)
		}
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, message(err.Error()))
			return
		}
		scopes[i] = Scope{Agent: canonicalAgent(sq.Query.Target), Contract: sq.Query.Contract}
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
