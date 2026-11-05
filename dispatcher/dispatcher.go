// Package dispatcher is the node's HTTP API: the counterpart of Python's
// ibc.py (Flask app, /ibc/app and /stream) and dispatcher/navigator.py.
// docs/blockchain-api.md in the Python repo describes the protocol.
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
	"strconv"
	"strings"
	"time"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/consensus"
	"github.com/ouripoupko/ibc-go/execution"
)

// Actions handled by the dispatcher itself.
const (
	actionIsExistAgent = "is_exist_agent"
	actionGetContracts = "get_contracts"
	actionGetContract  = "get_contract"
	actionJoinContract = "join_contract"
	actionRead         = "contract_read"
	actionGetLedger    = "a2a_get_ledger"
)

// ledgerKeyWidth pads ledger indexes in a2a_get_ledger replies, so that
// sorting the keys as strings (as Python's consensus did) keeps their order.
const ledgerKeyWidth = 15

// maxBody bounds a request body.
const maxBody = 16 << 20

// keepAlive is the interval of the empty event stream messages.
var keepAlive = 10 * time.Second

// Dispatcher serves the HTTP API.
type Dispatcher struct {
	navigator *execution.Navigator
	consensus consensus.Protocol
	broker    *Broker
	sender    common.Sender
	done      <-chan struct{}
	mux       *http.ServeMux
}

// New returns a Dispatcher. sender makes the direct partner calls
// (join_contract). Event streams end when done is closed.
func New(navigator *execution.Navigator, protocol consensus.Protocol, broker *Broker,
	sender common.Sender, done <-chan struct{}) *Dispatcher {
	d := &Dispatcher{navigator: navigator, consensus: protocol, broker: broker, sender: sender, done: done}
	d.mux = http.NewServeMux()
	d.mux.HandleFunc("/ibc/app", d.handleApp)
	d.mux.HandleFunc("/ibc/app/{path...}", d.handleApp)
	d.mux.HandleFunc("GET /stream", d.handleStream)
	return d
}

// ServeHTTP adds the CORS headers (Python used Flask-CORS) and answers
// preflight requests.
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

// handleApp serves /ibc/app/<identity>/<contract>/<method>?action=<action>.
func (d *Dispatcher) handleApp(w http.ResponseWriter, r *http.Request) {
	var parts [3]string
	segments := strings.Split(r.PathValue("path"), "/")
	if len(segments) > 3 {
		writeJSON(w, http.StatusNotFound, message("not found"))
		return
	}
	copy(parts[:], segments)

	msg, err := readMessage(r)
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
			"action", record.Action, "contract", record.Contract, "method", record.Method)
	}
	reply, err := d.handle(r.Context(), record)
	if err != nil {
		slog.Warn("request failed", "hash", short(record.HashCode), "action", record.Action, "err", err)
		reply = message(err.Error())
	}
	writeJSON(w, http.StatusOK, reply)
}

// handle runs an action. Like Python, errors in the request itself come
// back as JSON values with status 200, not as HTTP errors.
func (d *Dispatcher) handle(ctx context.Context, record execution.Record) (any, error) {
	type route struct{ method, action string }
	switch (route{record.Type, record.Action}) {
	case route{http.MethodGet, actionIsExistAgent}:
		return d.navigator.AgentExists(record.Agent)

	case route{http.MethodGet, actionGetContracts}:
		return d.navigator.Contracts(record.Agent)

	case route{http.MethodGet, actionGetContract}:
		summary, ok, err := d.navigator.Contract(record.Agent, record.Contract)
		if err != nil || !ok {
			return map[string]any{}, err
		}
		return summary, nil

	case route{http.MethodPut, execution.ActionRegisterAgent}:
		// Not a consensus matter: straight to execution, as in Python.
		if err := d.navigator.Submit(record.Agent, record); err != nil {
			return nil, err
		}
		return record.HashCode, nil

	case route{http.MethodPut, execution.ActionDeployContract}:
		record.Contract = record.HashCode
		if err := d.consensus.Submit(record.Agent, record); err != nil {
			return nil, err
		}
		return record.HashCode, nil

	case route{http.MethodPut, actionJoinContract}:
		return d.joinContract(ctx, record)

	case route{http.MethodPut, execution.ActionConnect},
		route{http.MethodPut, execution.ActionReplyJoin},
		route{http.MethodPut, execution.ActionConsent},
		route{http.MethodPost, execution.ActionWrite}:
		if err := d.consensus.Submit(record.Agent, record); err != nil {
			return nil, err
		}
		return record.HashCode, nil

	case route{http.MethodPost, actionRead}:
		result, err := d.navigator.Read(record.Agent, record)
		if errors.Is(err, execution.ErrUnknownContract) {
			return message("no such contract"), nil
		}
		if err != nil {
			var ce *common.ContractError
			if errors.As(err, &ce) || errors.Is(err, common.ErrNotImplemented) {
				return err.Error(), nil // as Python returned str(e)
			}
			return nil, err
		}
		return result.Value, nil

	case route{http.MethodPost, actionGetLedger}:
		return d.getLedger(record)
	}
	return nil, fmt.Errorf("unknown action %q for %s", record.Action, record.Type)
}

// joinContract asks a partner's node to let this agent join a contract
// (Python navigator.join_contract). The outcome arrives later as
// a2a_reply_join. A contract this agent already has is answered with null.
func (d *Dispatcher) joinContract(ctx context.Context, record execution.Record) (any, error) {
	f := record.Message.Fields
	contract, _ := f["contract"].(string)
	address, _ := f["address"].(string)
	agent, _ := f["agent"].(string)
	if contract == "" || address == "" || agent == "" {
		return nil, errors.New("join_contract needs contract, address and agent")
	}
	have, err := d.navigator.ContractExists(record.Agent, contract)
	if err != nil {
		return nil, err
	}
	if have {
		slog.Warn("double join", "agent", record.Agent, "contract", contract)
		return nil, nil
	}
	myAddress, err := d.navigator.AgentAddress(record.Agent)
	if err != nil {
		return nil, err
	}
	partner := common.NewPartner(address, agent, myAddress, record.Agent, d.sender, nil)
	if err := partner.Connect(ctx, contract, f["profile"]); err != nil {
		return nil, err
	}
	return record.HashCode, nil
}

// getLedger serves a2a_get_ledger: the contract's records with index >= the
// requested index, keyed by zero-padded index.
func (d *Dispatcher) getLedger(record execution.Record) (any, error) {
	exists, err := d.navigator.ContractExists(record.Agent, record.Contract)
	if err != nil {
		return nil, err
	}
	if !exists {
		return message("no such contract"), nil
	}
	var from uint64
	if msg, ok := record.Message.Fields["msg"].(map[string]any); ok {
		if n, ok := msg["index"].(json.Number); ok {
			if v, err := strconv.ParseUint(n.String(), 10, 64); err == nil {
				from = v
			}
		}
	}
	entries, err := d.navigator.Ledger(record.Agent, record.Contract, from)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(entries))
	for _, e := range entries {
		out[fmt.Sprintf("%0*d", ledgerKeyWidth, e.Index)] = e.Record
	}
	return out, nil
}

// handleStream serves /stream?agent=<id>&contract=<id|”>&..., the
// Server-Sent Events stream of replies.
func (d *Dispatcher) handleStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	agents, contracts := q["agent"], q["contract"]
	scopes := make([]Scope, len(agents))
	for i, agent := range agents {
		scopes[i].Agent = agent
		if i < len(contracts) {
			scopes[i].Contract = contracts[i]
		}
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

// readMessage decodes a JSON body; like Flask's get_json, anything that is
// not declared as JSON gives a null message.
func readMessage(r *http.Request) (execution.Message, error) {
	var msg execution.Message
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		return msg, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return msg, fmt.Errorf("read body: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return msg, nil
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return msg, fmt.Errorf("invalid JSON body: %w", err)
	}
	return msg, nil
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
