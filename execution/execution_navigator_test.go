package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/identity"
)

// recordingOutput collects what the navigator publishes.
type recordingOutput struct {
	mu      sync.Mutex
	replies []Reply
	decided map[string]bool // record hash -> approved
	applied map[string]uint64
	signal  chan struct{}
}

func newRecordingOutput() *recordingOutput {
	return &recordingOutput{signal: make(chan struct{}, 1), decided: map[string]bool{}, applied: map[string]uint64{}}
}

func (o *recordingOutput) Publish(_ string, r Reply) {
	o.mu.Lock()
	o.replies = append(o.replies, r)
	o.mu.Unlock()
	select {
	case o.signal <- struct{}{}:
	default:
	}
}

func (o *recordingOutput) Decided(_, _ string, r Record, approved bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.decided[r.HashCode] = approved
}

func (o *recordingOutput) Applied(_, contract string, index uint64, _ string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.applied[contract] = index
}

// wait returns once at least n replies were published.
func (o *recordingOutput) wait(t *testing.T, n int) []Reply {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		o.mu.Lock()
		if len(o.replies) >= n {
			out := append([]Reply(nil), o.replies...)
			o.mu.Unlock()
			return out
		}
		o.mu.Unlock()
		select {
		case <-o.signal:
		case <-deadline:
			t.Fatalf("timed out waiting for %d replies, have %d", n, len(o.replies))
		}
	}
}

const node = "http://node.example"

func registerRecord(t *testing.T, hash string) Record {
	return record(t, fmt.Sprintf(`{"type":"PUT","action":"register_agent","agent":%q,
		"message":{"address":%q},"timestamp":"20240102030000000000","hash_code":%q}`, alice, node, hash))
}

func deployRecord(t *testing.T, ctor string) Record {
	msg := deployMessage(t)
	msg[common.KeyConstructor] = json.RawMessage(ctor)
	msg["name"] = "registry"
	msg["protocol"] = "BFT"
	raw, err := json.Marshal(map[string]any{
		"type": "PUT", "action": ActionDeployContract, "agent": alice, "message": msg,
		"timestamp": createdAt, "hash_code": contract, "index": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record(t, string(raw))
}

func indexed(r Record, index uint64) Record {
	r.Index = index
	r.HashCode = fmt.Sprintf("h%d", index)
	return r
}

func newNavigator(t *testing.T, db ethdb.Database, out Output) *Navigator {
	t.Helper()
	n := NewNavigator(Config{DB: db, Output: out})
	t.Cleanup(n.Close)
	return n
}

func mustSubmit(t *testing.T, n *Navigator, r Record) {
	t.Helper()
	if err := n.Submit(alice, r); err != nil {
		t.Fatalf("Submit %s #%d: %v", r.Action, r.Index, err)
	}
}

// deployed registers alice and deploys the registry contract.
func deployed(t *testing.T, n *Navigator, out *recordingOutput) {
	t.Helper()
	mustSubmit(t, n, registerRecord(t, "reg"))
	mustSubmit(t, n, deployRecord(t, `{"initialName":"registry","start":7}`))
	replies := out.wait(t, 2)
	if replies[0].Reply != true || replies[0].Contract != nil {
		t.Fatalf("register reply = %+v", replies[0])
	}
	if replies[1].Reply != true || *replies[1].Contract != contract {
		t.Fatalf("deploy reply = %+v", replies[1])
	}
}

func contractDoc(db ethdb.Database) common.Document {
	return common.NewKVStore(db).Collection(AgentsCollection).Document(alice).
		GetSubCollection(ContractsCollection).Document(contract)
}

func TestNavigatorAppliesRecordsInOrder(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	out := newRecordingOutput()
	n := newNavigator(t, db, out)
	deployed(t, n, out)

	for i := uint64(2); i <= 21; i++ {
		mustSubmit(t, n, indexed(writeRecord(t, bob, "setValue", fmt.Sprintf(`{"value": %d}`, i), "20240102030500000000"), i))
	}
	replies := out.wait(t, 22)
	for i, r := range replies[2:] {
		if want := json.Number(fmt.Sprint(8 + i)); r.Reply != want {
			t.Fatalf("write #%d replied %v, want %v (records applied out of order?)", i+2, r.Reply, want)
		}
		if r.Request != fmt.Sprintf("h%d", i+2) || r.StateRoot == "" {
			t.Fatalf("reply = %+v", r)
		}
	}
	// The contract document keeps every deploy field, plus the cursor.
	fields, _ := contractDoc(db).Fields()
	if fields["name"] != "registry" || fields["protocol"] != "BFT" || fields[common.KeyBlockNumber] != json.Number("21") {
		t.Fatalf("contract document: name=%v protocol=%v block=%v", fields["name"], fields["protocol"], fields[common.KeyBlockNumber])
	}
	if length, _ := common.NewBlockChain(db, alice).Contract(contract).Len(); length != 21 {
		t.Fatalf("ledger length %d, want 21", length)
	}
}

func TestNavigatorIgnoresRedeliveryAndRejectsGaps(t *testing.T) {
	out := newRecordingOutput()
	n := newNavigator(t, rawdb.NewMemoryDatabase(), out)
	deployed(t, n, out)

	w := indexed(writeRecord(t, bob, "setValue", `{"value": 1}`, "20240102030500000000"), 2)
	mustSubmit(t, n, w)
	mustSubmit(t, n, w)                                                // redelivered: ignored
	mustSubmit(t, n, deployRecord(t, `{"initialName":"x","start":0}`)) // redelivered deploy: ignored
	if err := n.Submit(alice, indexed(w, 5)); !errors.Is(err, common.ErrLedgerGap) {
		t.Fatalf("gap: err = %v", err)
	}
	mustSubmit(t, n, indexed(writeRecord(t, bob, "setValue", `{"value": 2}`, "20240102030500000000"), 3))
	replies := out.wait(t, 4)
	time.Sleep(50 * time.Millisecond)
	if got := out.wait(t, 4); len(got) != 4 {
		t.Fatalf("%d replies, want 4: %+v", len(got), got)
	}
	if replies[2].Reply != json.Number("8") || replies[3].Reply != json.Number("9") {
		t.Fatalf("replies = %+v", replies[2:])
	}
}

func TestNavigatorRejectsUnknownTargets(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	n := newNavigator(t, db, nil)

	if err := n.Submit(alice, deployRecord(t, `{}`)); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("deploy for unregistered agent: %v", err)
	}
	mustSubmit(t, n, registerRecord(t, "reg"))
	if err := n.Submit(alice, writeRecord(t, bob, "setValue", `{"value": 1}`, createdAt)); !errors.Is(err, ErrUnknownContract) {
		t.Errorf("write to unknown contract: %v", err)
	}
	if err := n.Submit(alice, Record{Action: "contract_read", Contract: contract}); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("unknown action: %v", err)
	}
	if length, _ := common.NewBlockChain(db, alice).Contract(contract).Len(); length != 0 {
		t.Errorf("rejected records were logged: %d", length)
	}
}

func TestNavigatorReportsDecisionsAndProgress(t *testing.T) {
	out := newRecordingOutput()
	n := newNavigator(t, rawdb.NewMemoryDatabase(), out)
	deployed(t, n, out)

	carol, _ := crypto.GenerateKey()
	nodeKey, _ := crypto.GenerateKey()
	join := signedRecord(t, carol, identity.KindJoin, contract, joinBody(nodeKey), "20240102030500000000")
	join.Index = 2
	mustSubmit(t, n, join)
	replies := out.wait(t, 3)
	if replies[2].Reply != true || replies[2].Action != ActionJoin {
		t.Fatalf("join reply = %+v", replies[2])
	}
	out.mu.Lock()
	approved, ok := out.decided[join.HashCode]
	applied := out.applied[contract]
	out.mu.Unlock()
	if !ok || !approved {
		t.Fatalf("decision reported = %v, %v", approved, ok)
	}
	if applied != 2 {
		t.Fatalf("applied index reported = %d, want 2", applied)
	}
	if a, known, err := n.Decision(alice, contract, join.HashCode); err != nil || !known || !a {
		t.Fatalf("Decision = %v, %v, %v", a, known, err)
	}
}

func TestNavigatorFailedDeployIsStoredAndNotRetried(t *testing.T) {
	dir := t.TempDir()
	db, err := common.OpenPebble(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := newRecordingOutput()
	n := NewNavigator(Config{DB: db, Output: out})
	mustSubmit(t, n, registerRecord(t, "reg"))
	mustSubmit(t, n, deployRecord(t, `{"initialName":"registry"}`)) // missing "start"
	mustSubmit(t, n, indexed(writeRecord(t, bob, "setValue", `{"value": 1}`, createdAt), 2))
	replies := out.wait(t, 3)
	if s, _ := replies[1].Reply.(string); !strings.Contains(s, `missing argument "start"`) {
		t.Fatalf("deploy reply = %v", replies[1].Reply)
	}
	if s, _ := replies[2].Reply.(string); !strings.Contains(s, "contract deployment failed") {
		t.Fatalf("write reply = %v", replies[2].Reply)
	}
	n.Close()
	fields, _ := contractDoc(db).Fields()
	if fields[common.KeyBlockNumber] != json.Number("2") || fields[common.KeyEVMAddress] != nil {
		t.Fatalf("after failed deploy: block=%v evm_address=%v", fields[common.KeyBlockNumber], fields[common.KeyEVMAddress])
	}
	db.Close()

	// After a restart nothing is re-applied, and new records still get the stored failure.
	db, err = common.OpenPebble(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out = newRecordingOutput()
	n = newNavigator(t, db, out)
	if err := n.Recover(); err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, n, indexed(writeRecord(t, bob, "setValue", `{"value": 1}`, createdAt), 3))
	replies = out.wait(t, 1)
	if len(replies) != 1 || !strings.Contains(fmt.Sprint(replies[0].Reply), "contract deployment failed") {
		t.Fatalf("replies after restart = %+v", replies)
	}
}

// A crash after a record is logged but before it is applied (or after the
// state is committed but before the cursor moves) is repaired by Recover,
// which reaches the same root.
func TestNavigatorRecoverAfterCrash(t *testing.T) {
	dir := t.TempDir()
	db, err := common.OpenPebble(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := newRecordingOutput()
	n := NewNavigator(Config{DB: db, Output: out})
	deployed(t, n, out)
	mustSubmit(t, n, indexed(writeRecord(t, bob, "setValue", `{"value": 1}`, "20240102030500000000"), 2))
	out.wait(t, 3)
	n.Close()

	// Crash 1: write #3 was committed to the state but the document still
	// has the old cursor and root.
	before, _ := contractDoc(db).Fields()
	n = NewNavigator(Config{DB: db, Output: out})
	mustSubmit(t, n, indexed(writeRecord(t, bob, "setValue", `{"value": 2}`, "20240102030600000000"), 3))
	replies := out.wait(t, 4)
	wantRoot := replies[3].StateRoot
	n.Close()
	if err := contractDoc(db).Update(map[string]any{
		common.KeyBlockNumber:   before[common.KeyBlockNumber],
		common.KeyStateRoot:     before[common.KeyStateRoot],
		common.KeyLastTimestamp: before[common.KeyLastTimestamp],
	}); err != nil {
		t.Fatal(err)
	}
	// Crash 2: write #4 reached the ledger but was never applied.
	raw, _ := json.Marshal(indexed(writeRecord(t, bob, "setValue", `{"value": 3}`, "20240102030700000000"), 4))
	if _, err := common.NewBlockChain(db, alice).Contract(contract).Append(raw); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = common.OpenPebble(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out = newRecordingOutput()
	n = newNavigator(t, db, out)
	if err := n.Recover(); err != nil {
		t.Fatal(err)
	}
	replies = out.wait(t, 2)
	if replies[0].Request != "h3" || replies[0].StateRoot != wantRoot || replies[0].Reply != json.Number("9") {
		t.Fatalf("re-applied #3 = %+v, want root %s and reply 9", replies[0], wantRoot)
	}
	if replies[1].Request != "h4" || replies[1].Reply != json.Number("10") {
		t.Fatalf("applied #4 = %+v", replies[1])
	}
	fields, _ := contractDoc(db).Fields()
	if fields[common.KeyBlockNumber] != json.Number("4") ||
		ethcommon.HexToHash(fields[common.KeyStateRoot].(string)).Hex() != replies[1].StateRoot {
		t.Fatalf("document after recovery: block=%v root=%v", fields[common.KeyBlockNumber], fields[common.KeyStateRoot])
	}
}

func TestNavigatorClosesIdleContracts(t *testing.T) {
	out := newRecordingOutput()
	n := NewNavigator(Config{DB: rawdb.NewMemoryDatabase(), Output: out, IdleTimeout: 20 * time.Millisecond})
	t.Cleanup(n.Close)
	deployed(t, n, out)

	deadline := time.Now().Add(5 * time.Second)
	for n.activeWorkers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle contract goroutine was not closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mustSubmit(t, n, indexed(writeRecord(t, bob, "setValue", `{"value": 1}`, "20240102030500000000"), 2))
	if r := out.wait(t, 3)[2]; r.Reply != json.Number("8") {
		t.Fatalf("after reopening: reply %v, want 8", r.Reply)
	}
}

func TestNavigatorCloseDrainsSubmittedRecords(t *testing.T) {
	out := newRecordingOutput()
	n := NewNavigator(Config{DB: rawdb.NewMemoryDatabase(), Output: out})
	deployed(t, n, out)
	for i := uint64(2); i <= 51; i++ {
		mustSubmit(t, n, indexed(writeRecord(t, bob, "setValue", `{"value": 1}`, "20240102030500000000"), i))
	}
	n.Close()
	out.mu.Lock()
	defer out.mu.Unlock()
	if len(out.replies) != 52 {
		t.Fatalf("%d replies after Close, want 52", len(out.replies))
	}
	if err := n.Submit(alice, writeRecord(t, bob, "setValue", `{"value": 1}`, createdAt)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after Close: %v", err)
	}
}
