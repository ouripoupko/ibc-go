package consensus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/rawdb"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/execution"
)

var _ Protocol = (*Immediate)(nil)

const (
	alice    = "alice-pid"
	contract = "contract-1"
)

type output struct {
	mu      sync.Mutex
	replies []execution.Reply
}

func (o *output) Publish(_ string, r execution.Reply) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.replies = append(o.replies, r)
}

func (o *output) Partner(string, execution.Record) {}

func (o *output) wait(t *testing.T, n int) []execution.Reply {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		o.mu.Lock()
		got := append([]execution.Reply(nil), o.replies...)
		o.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d replies, have %d", n, len(got))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func decode(t *testing.T, raw string) execution.Record {
	t.Helper()
	r, err := execution.DecodeRecord([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func deployRecord(t *testing.T) execution.Record {
	t.Helper()
	bin, err := os.ReadFile("../testdata/Registry.bin")
	if err != nil {
		t.Fatal(err)
	}
	abiJSON, err := os.ReadFile("../testdata/Registry.abi")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "PUT", "action": "deploy_contract", "agent": alice, "hash_code": contract,
		"timestamp": "20240102030405000000",
		"message": map[string]any{
			"pid": alice, "address": "http://alice.example", "protocol": "BFT",
			"code": strings.TrimSpace(string(bin)), "abi": string(abiJSON),
			"constructor": map[string]any{"initialName": "registry", "start": 7},
		},
	})
	return decode(t, string(raw))
}

func writeRecord(t *testing.T, value int, index int) execution.Record {
	return decode(t, fmt.Sprintf(`{"type":"POST","action":"contract_write","contract":%q,"agent":"bob",
		"method":"setValue","message":{"values":{"value":%d}},"timestamp":"20240102030500000000",
		"hash_code":"w%d","index":%d}`, contract, value, value, index))
}

func TestImmediateCommitsRecordsAtOnce(t *testing.T) {
	out := &output{}
	nav := execution.NewNavigator(execution.Config{DB: rawdb.NewMemoryDatabase(),
		Sender: &common.RecordingSender{}, Output: out})
	defer nav.Close()
	p := NewImmediate(nav, out)
	defer p.Close()

	// register_agent goes to execution directly, not through consensus.
	if err := p.Submit(alice, decode(t, `{"action":"register_agent"}`)); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("register_agent through consensus: %v", err)
	}
	if err := nav.Submit(alice, decode(t, `{"action":"register_agent","message":{"address":"http://alice.example"},"hash_code":"r"}`)); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(alice, deployRecord(t)); err != nil {
		t.Fatal(err)
	}
	// Indexes from outside are ignored: records are ordered by arrival.
	for i, index := range []int{99, 0, 5} {
		if err := p.Submit(alice, writeRecord(t, i+1, index)); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
	replies := out.wait(t, 5)
	if replies[1].Reply != true {
		t.Fatalf("deploy reply = %+v", replies[1])
	}
	for i, r := range replies[2:] {
		if want := json.Number(fmt.Sprint(8 + i)); r.Reply != want || r.Request != fmt.Sprintf("w%d", i+1) {
			t.Fatalf("write reply %d = %+v, want %v", i+1, r, want)
		}
	}
}

func TestImmediateReplyJoinIsPublished(t *testing.T) {
	out := &output{}
	p := NewImmediate(nil, out)
	for _, status := range []bool{false, true} {
		err := p.Submit(alice, decode(t, fmt.Sprintf(`{"action":"a2a_reply_join","contract":%q,
			"message":{"from":"bob","to":%q,"msg":{"pid":"bob","address":"http://bob.example","status":%v}}}`,
			contract, alice, status)))
		if err != nil {
			t.Fatal(err)
		}
	}
	replies := out.wait(t, 2)
	for i, want := range []bool{false, true} {
		if replies[i].Action != "a2a_reply_join" || replies[i].Reply != want || replies[i].Contract != nil {
			t.Errorf("reply %d = %+v, want status %v", i, replies[i], want)
		}
	}
}

func TestImmediateRejectsProtocolMessages(t *testing.T) {
	p := NewImmediate(nil, nil)
	if err := p.Submit(alice, decode(t, `{"action":"a2a_consent","message":{"msg":{"step":"PREPARE"}}}`)); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("a2a_consent: %v", err)
	}
	if err := p.Submit(alice, decode(t, `{"action":"bogus"}`)); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("bogus action: %v", err)
	}
	p.PartnerUpdate(alice, execution.Record{Action: "int_partner"}) // no-op, must not panic
}
