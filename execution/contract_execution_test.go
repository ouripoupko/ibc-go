package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/common"
)

const (
	alice     = "alice-pid"
	bob       = "bob-pid"
	contract  = "contract-hash-1"
	createdAt = "20240102030405123456"
)

// agent is one partner running the contract: its own document, store,
// ledger and sender.
type agent struct {
	me     string
	doc    *common.MemoryDocument
	db     ethdb.Database
	ledger *MemoryLedger
	sender *common.RecordingSender
	exec   *ContractExecution
}

// deployMessage is the deploy record's message, which the navigator stores
// as the contract document.
func deployMessage(t *testing.T) map[string]any {
	t.Helper()
	bin, err := os.ReadFile("../testdata/Registry.bin")
	if err != nil {
		t.Fatal(err)
	}
	abiJSON, err := os.ReadFile("../testdata/Registry.abi")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		common.KeyPID:         alice,
		common.KeyAddress:     "http://alice.example",
		common.KeyProfile:     map[string]any{"name": "Alice"},
		common.KeyCode:        "0x" + strings.TrimSpace(string(bin)),
		common.KeyABI:         string(abiJSON),
		common.KeyConstructor: map[string]any{"initialName": "registry", "start": json.Number("7")},
	}
}

// newAgent creates the contract on a fresh agent, as deploy_contract does.
func newAgent(t *testing.T, me string) *agent {
	t.Helper()
	a := &agent{
		me:     me,
		doc:    common.NewMemoryDocument(deployMessage(t)),
		db:     rawdb.NewMemoryDatabase(),
		ledger: &MemoryLedger{},
		sender: &common.RecordingSender{},
	}
	a.exec = NewContractExecution(a.doc, contract, me, "http://"+me+".example", a.db, a.ledger, a.sender)
	t.Cleanup(a.exec.Close)
	ok, err := a.exec.Create(Record{
		Type: "PUT", Action: "deploy_contract", Agent: alice,
		Timestamp: createdAt, HashCode: contract,
	})
	if err != nil || !ok {
		t.Fatalf("Create = %v, %v", ok, err)
	}
	return a
}

func record(t *testing.T, raw string) Record {
	t.Helper()
	r, err := DecodeRecord([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func writeRecord(t *testing.T, agentID, method, values, timestamp string) Record {
	return record(t, fmt.Sprintf(`{"type":"POST","action":"contract_write","contract":%q,
		"agent":%q,"method":%q,"message":{"values":%s},"timestamp":%q,"hash_code":"h"}`,
		contract, agentID, method, values, timestamp))
}

func viewRecord(t *testing.T, agentID, method, values string) Record {
	return record(t, fmt.Sprintf(`{"agent":%q,"method":%q,"message":{"values":%s}}`,
		agentID, method, values))
}

func (a *agent) view(t *testing.T, method, values string) any {
	t.Helper()
	res, err := a.exec.Call(viewRecord(t, a.me, method, values), false)
	if err != nil {
		t.Fatalf("view %s: %v", method, err)
	}
	return res.Value
}

func (a *agent) storedRoot(t *testing.T) ethcommon.Hash {
	t.Helper()
	v, _ := a.doc.Get(common.KeyStateRoot)
	s, ok := v.(string)
	if !ok {
		t.Fatalf("state_root = %v", v)
	}
	return ethcommon.HexToHash(s)
}

// 1. Deployment with a constructor argument.
func TestDeployWithConstructorArguments(t *testing.T) {
	a := newAgent(t, alice)

	if got := a.view(t, "name", `{}`); got != "registry" {
		t.Errorf("name = %v, want registry", got)
	}
	if got := a.view(t, "counter", `{}`); got != json.Number("7") {
		t.Errorf("counter = %v, want 7", got)
	}
	addr, _ := a.doc.Get(common.KeyEVMAddress)
	if addr != a.exec.State().Address().Hex() || a.exec.State().Address() == (ethcommon.Address{}) {
		t.Errorf("evm_address = %v", addr)
	}
	if a.storedRoot(t) != a.exec.State().Root() {
		t.Errorf("stored root %s != state root %s", a.storedRoot(t), a.exec.State().Root())
	}
	if id, _ := a.doc.Get("id"); id != contract {
		t.Errorf("id = %v", id)
	}
	// The creator becomes the first partner, as in Python.
	partner, _ := a.doc.GetSubCollection(PartnersCollection).Document(alice).Fields()
	ok, _ := a.doc.GetSubCollection(PartnersCollection).Has(alice)
	if !ok || partner["address"] != "http://alice.example" {
		t.Errorf("creator partner = %v, %v", partner, ok)
	}
	if n := len(a.ledger.All()); n != 1 {
		t.Errorf("ledger has %d records, want 1", n)
	}
}

// 2. A write call changes the state and the root; msg.sender is the agent's address.
func TestWriteCallChangesStateAndRoot(t *testing.T) {
	a := newAgent(t, alice)
	before := a.exec.State().Root()
	ts := "20240102030500000000"

	res, err := a.exec.Call(writeRecord(t, bob, "setValue", `{"value": 42}`, ts), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Value != json.Number("8") {
		t.Errorf("setValue returned %v, want 8", res.Value)
	}
	if res.StateRoot == before {
		t.Error("root did not change")
	}
	if res.StateRoot != a.storedRoot(t) || res.StateRoot != a.exec.State().Root() {
		t.Errorf("returned root %s, stored %s", res.StateRoot, a.storedRoot(t))
	}
	if bn, _ := a.doc.Get(common.KeyBlockNumber); bn != uint64(2) {
		t.Errorf("block_number = %v, want 2", bn)
	}

	bobAddr := common.AgentAddress(bob)
	if got := a.view(t, "getValue", fmt.Sprintf(`{"who": %q}`, bobAddr.Hex())); got != json.Number("42") {
		t.Errorf("getValue(bob) = %v, want 42", got)
	}
	info, ok := a.view(t, "info", `{}`).([]any)
	if !ok || len(info) != 4 {
		t.Fatalf("info = %v", info)
	}
	if info[3] != bobAddr.Hex() {
		t.Errorf("msg.sender = %v, want AgentAddress(bob) = %s", info[3], bobAddr.Hex())
	}
	wantTime, _ := common.ParseTimestamp(ts)
	if info[2] != json.Number(fmt.Sprint(wantTime)) {
		t.Errorf("block.timestamp = %v, want %d", info[2], wantTime)
	}
	if n := len(a.ledger.All()); n != 2 {
		t.Errorf("ledger has %d records, want 2", n)
	}
}

// 3. A view call changes neither the state nor the root.
func TestViewCallChangesNothing(t *testing.T) {
	a := newAgent(t, alice)
	if _, err := a.exec.Call(writeRecord(t, bob, "setValue", `{"value": 1}`, "20240102030500000000"), true); err != nil {
		t.Fatal(err)
	}
	root := a.exec.State().Root()
	doc := a.doc.Snapshot()
	records := len(a.ledger.All())

	if got := a.view(t, "counter", `{}`); got != json.Number("8") {
		t.Errorf("counter = %v, want 8", got)
	}
	// Even a state-changing method, called read-only, is discarded (eth_call).
	res, err := a.exec.Call(viewRecord(t, bob, "setValue", `{"value": 99}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Value != json.Number("9") || res.StateRoot != root {
		t.Errorf("read-only setValue = %v root %s", res.Value, res.StateRoot)
	}

	if a.exec.State().Root() != root {
		t.Error("root changed")
	}
	if !reflect.DeepEqual(a.doc.Snapshot(), doc) {
		t.Errorf("document changed: %v -> %v", doc, a.doc.Snapshot())
	}
	if got := a.view(t, "counter", `{}`); got != json.Number("8") {
		t.Errorf("counter after views = %v, want 8", got)
	}
	if got := a.view(t, "getValue", fmt.Sprintf(`{"who": %q}`, common.AgentAddress(bob).Hex())); got != json.Number("1") {
		t.Errorf("getValue(bob) = %v, want 1", got)
	}
	if n := len(a.ledger.All()); n != records {
		t.Errorf("view calls were logged")
	}
}

// 4. A revert leaves the state and the root as they were, and returns the reason.
func TestRevertRollsBackEverything(t *testing.T) {
	a := newAgent(t, alice)
	if _, err := a.exec.Call(writeRecord(t, bob, "setValue", `{"value": 5}`, "20240102030500000000"), true); err != nil {
		t.Fatal(err)
	}
	root := a.exec.State().Root()
	info := a.view(t, "info", `{}`)

	res, err := a.exec.Call(writeRecord(t, bob, "setThenFail", `{"value": 6}`, "20240102030600000000"), true)
	var ce *common.ContractError
	if !errors.As(err, &ce) || !errors.Is(err, vm.ErrExecutionReverted) {
		t.Fatalf("err = %v, want reverted ContractError", err)
	}
	if ce.Reason != "setThenFail: rejected" || !strings.Contains(err.Error(), "setThenFail: rejected") {
		t.Errorf("reason = %q, error = %q", ce.Reason, err)
	}
	if res.StateRoot != root || a.exec.State().Root() != root || a.storedRoot(t) != root {
		t.Error("root changed after revert")
	}
	if got := a.view(t, "info", `{}`); fmt.Sprint(got) != fmt.Sprint(info) {
		t.Errorf("state changed after revert: %v -> %v", info, got)
	}
	if got := a.view(t, "getValue", fmt.Sprintf(`{"who": %q}`, common.AgentAddress(bob).Hex())); got != json.Number("5") {
		t.Errorf("getValue(bob) = %v, want 5", got)
	}
	// The record was logged, so the block number still advances.
	if bn, _ := a.doc.Get(common.KeyBlockNumber); bn != uint64(3) {
		t.Errorf("block_number = %v, want 3", bn)
	}
}

// 5. Two partners running the same records reach identical roots at every step.
func TestDeterminismAcrossPartners(t *testing.T) {
	a := newAgent(t, alice)
	b := newAgent(t, bob)
	if a.exec.State().Root() != b.exec.State().Root() {
		t.Fatalf("roots differ after deploy: %s vs %s", a.exec.State().Root(), b.exec.State().Root())
	}
	if a.exec.State().Address() != b.exec.State().Address() {
		t.Fatal("contract addresses differ")
	}

	type step struct {
		rec       Record
		shouldLog bool
	}
	steps := []step{
		{writeRecord(t, alice, "setValue", `{"value": 10}`, "20240102030500000000"), true},
		{writeRecord(t, bob, "setValue", `{"value": 20}`, "20240102030510000000"), true},
		{writeRecord(t, bob, "setThenFail", `{"value": 30}`, "20240102030520000000"), true},
		{viewRecord(t, alice, "info", `{}`), false},
		{writeRecord(t, alice, "setValue", `{"value": "1000000000000000000000"}`, "20240102030530000000"), true},
		{writeRecord(t, bob, "nope", `{}`, "20240102030540000000"), true},
		{writeRecord(t, alice, "setValue", `{"value": 11}`, "20240102030550123456"), true},
	}
	roots := map[ethcommon.Hash]bool{a.exec.State().Root(): true}
	for i, s := range steps {
		ra, errA := a.exec.Call(s.rec, s.shouldLog)
		rb, errB := b.exec.Call(s.rec, s.shouldLog)
		if fmt.Sprint(errA) != fmt.Sprint(errB) {
			t.Fatalf("step %d: errors differ: %v vs %v", i, errA, errB)
		}
		if ra.StateRoot != rb.StateRoot || a.exec.State().Root() != b.exec.State().Root() {
			t.Fatalf("step %d: roots differ: %s vs %s", i, ra.StateRoot, rb.StateRoot)
		}
		if ra.BlockNumber != rb.BlockNumber || fmt.Sprint(ra.Value) != fmt.Sprint(rb.Value) {
			t.Fatalf("step %d: results differ: %+v vs %+v", i, ra, rb)
		}
		roots[ra.StateRoot] = true
	}
	// deploy + 4 successful writes give 5 distinct roots.
	if len(roots) != 5 {
		t.Errorf("saw %d distinct roots, want 5", len(roots))
	}
	if !reflect.DeepEqual(a.doc.Snapshot(), b.doc.Snapshot()) {
		t.Errorf("documents differ:\n%v\n%v", a.doc.Snapshot(), b.doc.Snapshot())
	}
}

// 6. The built-in stubs return "not implemented" and change nothing.
func TestBuiltinStubsNotImplemented(t *testing.T) {
	a := newAgent(t, alice)
	root := a.exec.State().Root()
	doc := a.doc.Snapshot()

	for _, method := range []string{common.MethodGetPartners, common.MethodApprovePartner, common.MethodGetParameters} {
		for _, shouldLog := range []bool{true, false} {
			res, err := a.exec.Call(writeRecord(t, bob, method, `{}`, "20240102030500000000"), shouldLog)
			if !errors.Is(err, common.ErrNotImplemented) {
				t.Errorf("%s (log=%v): err = %v, want ErrNotImplemented", method, shouldLog, err)
			}
			if res.StateRoot != root {
				t.Errorf("%s (log=%v): returned root %s, want %s", method, shouldLog, res.StateRoot, root)
			}
		}
	}
	if a.exec.State().Root() != root {
		t.Error("root changed")
	}
	// The three logged calls are ledger records, so each advances the block
	// number (the cursor); nothing else in the document changes.
	after := a.doc.Snapshot()
	if after[common.KeyBlockNumber] != uint64(4) {
		t.Errorf("block_number = %v, want 4 (deploy + 3 logged stub calls)", after[common.KeyBlockNumber])
	}
	delete(after, common.KeyBlockNumber)
	delete(doc, common.KeyBlockNumber)
	if !reflect.DeepEqual(after, doc) {
		t.Errorf("document changed: %v -> %v", doc, after)
	}
	if n := len(a.ledger.All()); n != 4 {
		t.Errorf("ledger has %d records, want 4", n)
	}
}

func TestJoinNotApprovedUntilApprovePartnerIsDefined(t *testing.T) {
	a := newAgent(t, alice)
	root := a.exec.State().Root()
	join := record(t, `{"type":"PUT","action":"a2a_connect","contract":"contract-hash-1",
		"message":{"from":"carol-pid","to":"alice-pid",
		"msg":{"pid":"carol-pid","address":"http://carol.example","profile":{"name":"Carol"}}},
		"timestamp":"20240102030500000000","hash_code":"j1"}`)

	approved, err := a.exec.Join(join)
	if err != nil || approved {
		t.Fatalf("Join = %v, %v; want false, nil", approved, err)
	}
	if ok, _ := a.doc.GetSubCollection(PartnersCollection).Has("carol-pid"); ok {
		t.Error("carol was added as a partner")
	}
	if a.exec.State().Root() != root {
		t.Error("join changed the root")
	}

	// An existing partner is not approved again; a join addressed to another
	// agent sends no reply.
	if approved, err := a.exec.Join(record(t, `{"message":{"to":"someone-else",
		"msg":{"pid":"alice-pid","address":"http://alice.example"}}}`)); err != nil || approved {
		t.Fatalf("Join(existing) = %v, %v", approved, err)
	}

	a.exec.Close() // flushes the reply queue
	reqs := a.sender.Requests()
	if len(reqs) != 1 {
		t.Fatalf("sent %d replies, want 1: %+v", len(reqs), reqs)
	}
	want := common.Request{
		Method: "PUT",
		URL:    "http://carol.example/ibc/app/carol-pid/" + contract,
		Params: map[string]string{"action": "a2a_reply_join"},
		JSON: map[string]any{"from": alice, "to": "carol-pid",
			"msg": map[string]any{"address": "http://alice-pid.example", "pid": alice, "status": false}},
	}
	if fmt.Sprint(reqs[0]) != fmt.Sprint(want) {
		t.Errorf("reply = %+v\nwant    %+v", reqs[0], want)
	}
	if n := len(a.ledger.All()); n != 3 {
		t.Errorf("ledger has %d records, want 3", n)
	}
	if _, err := a.exec.Join(join); !errors.Is(err, common.ErrOutboxClosed) {
		t.Errorf("Join after Close: err = %v, want ErrOutboxClosed", err)
	}
}

func TestReloadAfterRestart(t *testing.T) {
	a := newAgent(t, alice)
	if _, err := a.exec.Call(writeRecord(t, bob, "setValue", `{"value": 3}`, "20240102030500000000"), true); err != nil {
		t.Fatal(err)
	}
	root := a.exec.State().Root()
	a.exec.Close()

	// As navigator.get_contract does: a new ContractExecution, then Run.
	reloaded := NewContractExecution(a.doc, contract, alice, "http://alice.example", a.db, a.ledger, a.sender)
	defer reloaded.Close()
	if err := reloaded.Run(); err != nil {
		t.Fatal(err)
	}
	if reloaded.State().Root() != root {
		t.Fatalf("root after reload = %s, want %s", reloaded.State().Root(), root)
	}
	res, err := reloaded.Call(viewRecord(t, alice, "counter", `{}`), false)
	if err != nil || res.Value != json.Number("8") {
		t.Fatalf("counter after reload = %v, %v; want 8 (constructor must not rerun)", res.Value, err)
	}
}

func TestContractsDoNotShareState(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	open := func(hash string) *ContractExecution {
		doc := common.NewMemoryDocument(deployMessage(t))
		c := NewContractExecution(doc, hash, alice, "http://alice.example", db, &MemoryLedger{}, &common.RecordingSender{})
		t.Cleanup(c.Close)
		if _, err := c.Create(Record{Agent: alice, Timestamp: createdAt}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1, c2 := open("contract-1"), open("contract-2")
	if _, err := c1.Call(writeRecord(t, bob, "setValue", `{"value": 1}`, "20240102030500000000"), true); err != nil {
		t.Fatal(err)
	}
	res, err := c2.Call(viewRecord(t, bob, "counter", `{}`), false)
	if err != nil || res.Value != json.Number("7") {
		t.Fatalf("contract-2 counter = %v, %v; want 7", res.Value, err)
	}
	if c1.State().Root() == c2.State().Root() {
		t.Fatal("roots are equal after a write to only one contract")
	}
}

// The whole stack on Pebble: documents, ledger and EVM state in one store,
// surviving a close and reopen of the database.
func TestPebbleEndToEndAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() (ethdb.Database, common.Document, *common.ContractLedger) {
		db, err := common.OpenPebble(dir)
		if err != nil {
			t.Fatal(err)
		}
		agentDoc := common.NewKVStore(db).Collection("agents").Document(alice)
		doc := agentDoc.GetSubCollection("contracts").Document(contract)
		return db, doc, common.NewBlockChain(db, alice).Contract(contract)
	}

	db, doc, ledger := open()
	if err := doc.Update(deployMessage(t)); err != nil { // as deploy_contract stores the message
		t.Fatal(err)
	}
	c := NewContractExecution(doc, contract, alice, "http://alice.example", db, ChainLedger{ledger}, &common.RecordingSender{})
	if ok, err := c.Create(Record{Agent: alice, Timestamp: createdAt, HashCode: contract}); err != nil || !ok {
		t.Fatalf("Create = %v, %v", ok, err)
	}
	res, err := c.Call(writeRecord(t, bob, "setValue", `{"value": "12345678901234567890"}`, "20240102030500000000"), true)
	if err != nil {
		t.Fatal(err)
	}
	root := res.StateRoot
	if _, err := c.Join(record(t, `{"message":{"to":"alice-pid",
		"msg":{"pid":"carol-pid","address":"http://carol.example"}},"timestamp":"20240102030600000000"}`)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, doc, ledger = open()
	defer db.Close()
	c = NewContractExecution(doc, contract, alice, "http://alice.example", db, ChainLedger{ledger}, &common.RecordingSender{})
	defer c.Close()
	if err := c.Run(); err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if c.State().Root() != root || c.State().BlockNumber() != 3 {
		t.Fatalf("after restart: root %s block %d, want %s block 3 (= ledger length)", c.State().Root(), c.State().BlockNumber(), root)
	}
	got, err := c.Call(viewRecord(t, bob, "getValue", fmt.Sprintf(`{"who": %q}`, common.AgentAddress(bob).Hex())), false)
	if err != nil || got.Value != json.Number("12345678901234567890") {
		t.Fatalf("getValue after restart = %v, %v", got.Value, err)
	}
	if ok, err := doc.GetSubCollection(PartnersCollection).Has(alice); err != nil || !ok {
		t.Fatalf("creator partner after restart: %v, %v", ok, err)
	}
	if n, err := ledger.Len(); err != nil || n != 3 {
		t.Fatalf("ledger length after restart = %d, %v; want 3 (deploy, write, join)", n, err)
	}
	first, _, _ := ledger.Get(1)
	if r, err := DecodeRecord(first); err != nil || r.HashCode != contract {
		t.Fatalf("first ledger record = %s, %v", first, err)
	}
}
