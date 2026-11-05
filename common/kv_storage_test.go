package common

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
)

func TestKVDocumentFieldsAndUpdate(t *testing.T) {
	store := NewKVStore(rawdb.NewMemoryDatabase())
	agents := store.Collection("agents")
	doc := agents.Document("alice")

	if ok, err := doc.Exists(); err != nil || ok {
		t.Fatalf("Exists before write = %v, %v", ok, err)
	}
	if f, err := doc.Fields(); err != nil || len(f) != 0 {
		t.Fatalf("Fields before write = %v, %v", f, err)
	}
	if err := doc.Update(map[string]any{"address": "http://a", "n": 1, "big": json.Number("123456789012345678901234567890")}); err != nil {
		t.Fatal(err)
	}
	if err := doc.Update(map[string]any{"n": 2, "list": []any{"x", true}}); err != nil {
		t.Fatal(err)
	}
	got, err := agents.Document("alice").Fields()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"address": "http://a",
		"n":       json.Number("2"),
		"big":     json.Number("123456789012345678901234567890"),
		"list":    []any{"x", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Fields = %#v\nwant     %#v", got, want)
	}
	if ok, err := agents.Has("alice"); err != nil || !ok {
		t.Fatalf("Has = %v, %v", ok, err)
	}
}

func TestKVCollectionKeysAreDirectChildrenOnly(t *testing.T) {
	store := NewKVStore(rawdb.NewMemoryDatabase())
	agents := store.Collection("agents")
	for _, k := range []string{"bob", "alice", "a/b", "nul\x00byte"} {
		if err := agents.Document(k).Update(map[string]any{"k": k}); err != nil {
			t.Fatal(err)
		}
	}
	contracts := agents.Document("alice").GetSubCollection("contracts")
	if err := contracts.Document("c1").Update(map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	// A root collection whose name looks like a nested path must not mix in.
	if err := store.Collection("agents/alice").Document("zzz").Update(map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}

	keys, err := agents.Keys()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b", "alice", "bob", "nul\x00byte"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("agents keys = %q, want %q", keys, want)
	}
	keys, err = contracts.Keys()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c1"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("contracts keys = %q, want %q", keys, want)
	}
	if f, _ := agents.Document("nul\x00byte").Fields(); f["k"] != "nul\x00byte" {
		t.Fatalf("document with NUL in key = %v", f)
	}
}

func TestKVStorePersistsInPebble(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenPebble(dir)
	if err != nil {
		t.Fatal(err)
	}
	doc := NewKVStore(db).Collection("agents").Document("alice")
	if err := doc.GetSubCollection("contracts").Document("c1").Update(map[string]any{"pid": "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenPebble(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, err := NewKVStore(db).Collection("agents").Document("alice").
		GetSubCollection("contracts").Document("c1").Fields()
	if err != nil || f["pid"] != "alice" {
		t.Fatalf("after reopen: %v, %v", f, err)
	}
}

func TestContractDatabaseIsolation(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	tables := map[string]ethdb.Database{
		"alice/c":  ContractDatabase(db, "alice", "c"),
		"alice/c/": ContractDatabase(db, "alice", "c/"),
		"alic/ec":  ContractDatabase(db, "alic", "ec"),
		"bob/c":    ContractDatabase(db, "bob", "c"),
	}
	for name, table := range tables {
		if err := table.Put([]byte("key"), []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, table := range tables {
		v, err := table.Get([]byte("key"))
		if err != nil || string(v) != name {
			t.Errorf("%s: got %q, %v", name, v, err)
		}
	}
}
