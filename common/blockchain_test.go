package common

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
)

func collect(t *testing.T, l *ContractLedger, from uint64) []string {
	t.Helper()
	var out []string
	err := l.Records(from, func(index uint64, record json.RawMessage) error {
		out = append(out, fmt.Sprintf("%d:%s", index, record))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLedgerAppendAndRead(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain := NewBlockChain(db, "alice")
	l := chain.Contract("c1")
	if chain.Contract("c1") != l {
		t.Fatal("Contract returned a different instance for the same contract")
	}
	for i := 1; i <= 300; i++ { // crosses a byte boundary in the index
		idx, err := l.Append(json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)))
		if err != nil || idx != uint64(i) {
			t.Fatalf("Append #%d = %d, %v", i, idx, err)
		}
	}
	if n, err := l.Len(); err != nil || n != 300 {
		t.Fatalf("Len = %d, %v", n, err)
	}
	got := collect(t, l, 298)
	if want := []string{`298:{"i":298}`, `299:{"i":299}`, `300:{"i":300}`}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Records(298) = %v", got)
	}
	if all := collect(t, l, 0); len(all) != 300 || all[0] != `1:{"i":1}` || all[255] != `256:{"i":256}` {
		t.Fatalf("Records(0): %d records, first %q, 256th %q", len(all), all[0], all[255])
	}
	if r, ok, err := l.Get(7); err != nil || !ok || string(r) != `{"i":7}` {
		t.Fatalf("Get(7) = %s, %v, %v", r, ok, err)
	}
	if _, ok, err := l.Get(301); err != nil || ok {
		t.Fatalf("Get(301) = %v, %v", ok, err)
	}
	if _, err := l.Append(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("Append accepted invalid JSON")
	}
}

func TestLedgersAreSeparatePerContractAndAgent(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	a1 := NewBlockChain(db, "alice").Contract("c1")
	a2 := NewBlockChain(db, "alice").Contract("c2")
	b1 := NewBlockChain(db, "bob").Contract("c1")
	for _, l := range []*ContractLedger{a1, a1, a2, b1} {
		if _, err := l.Append(json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		l    *ContractLedger
		want uint64
	}{"alice/c1": {a1, 2}, "alice/c2": {a2, 1}, "bob/c1": {b1, 1}} {
		if n, _ := tc.l.Len(); n != tc.want {
			t.Errorf("%s: Len = %d, want %d", name, n, tc.want)
		}
	}
}

func TestLedgerPersistsInPebble(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenPebble(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := NewBlockChain(db, "alice").Contract("c1").Append(json.RawMessage(`"r"`)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	db, err = OpenPebble(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l := NewBlockChain(db, "alice").Contract("c1")
	if idx, err := l.Append(json.RawMessage(`"r"`)); err != nil || idx != 4 {
		t.Fatalf("Append after reopen = %d, %v; want 4", idx, err)
	}
}
