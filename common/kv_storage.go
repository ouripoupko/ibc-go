package common

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
)

// One key-value store (Pebble on disk) holds everything an ibc node keeps,
// under separate key prefixes:
//
//	"ibc/doc/"    documents and collections (KVStore)
//	"ibc/ledger/" ledgers (BlockChain)
//	"ibc/evm/"    each contract's EVM state (ContractDatabase)
//
// Variable-length key parts are length-prefixed, so no id can reach into
// another id's key range, whatever characters it contains.
var (
	docPrefix    = []byte("ibc/doc/")
	ledgerPrefix = []byte("ibc/ledger/")
	evmPrefix    = []byte("ibc/evm/")
)

// OpenPebble opens (or creates) the Pebble database in dir.
func OpenPebble(dir string) (ethdb.Database, error) {
	kv, err := pebble.New(dir, 64, 64, "", false)
	if err != nil {
		return nil, fmt.Errorf("open pebble %s: %w", dir, err)
	}
	return rawdb.NewDatabase(kv), nil
}

// ContractDatabase returns the part of db that holds one contract's EVM
// state for one agent. Contracts never share state, and neither do two
// agents on the same node that are partners in the same contract.
func ContractDatabase(db ethdb.Database, agent, contract string) ethdb.Database {
	return rawdb.NewTable(db, string(appendParts(slices.Clone(evmPrefix), agent, contract)))
}

// appendParts appends each part as uvarint(len) followed by its bytes.
func appendParts(key []byte, parts ...string) []byte {
	for _, p := range parts {
		key = binary.AppendUvarint(key, uint64(len(p)))
		key = append(key, p...)
	}
	return key
}

// KVStore keeps Documents and Collections in an ethdb.KeyValueStore. Each
// document is one JSON value, so updating a document is atomic.
type KVStore struct {
	db ethdb.KeyValueStore
	mu sync.Mutex // serialises read-modify-write updates
}

// NewKVStore returns a KVStore over db.
func NewKVStore(db ethdb.KeyValueStore) *KVStore {
	return &KVStore{db: db}
}

// Collection returns the root collection with the given name (Python's
// get_root_collection returned 'agents').
func (s *KVStore) Collection(name string) Collection {
	return &kvCollection{store: s, path: []string{name}}
}

type kvCollection struct {
	store *KVStore
	path  []string // collection, document, collection, document, ..., collection
}

// prefix is the key prefix shared by the collection's direct documents only:
// the path length comes first, so documents of nested collections (longer
// paths) never share it.
func (c *kvCollection) prefix() []byte {
	key := binary.AppendUvarint(slices.Clone(docPrefix), uint64(len(c.path)))
	return appendParts(key, c.path...)
}

func (c *kvCollection) Document(key string) Document {
	return &kvDocument{collection: c, key: key}
}

func (c *kvCollection) Has(key string) (bool, error) {
	return c.Document(key).Exists()
}

func (c *kvCollection) Delete(key string) error {
	return c.store.db.Delete(c.Document(key).(*kvDocument).dbKey())
}

func (c *kvCollection) Keys() ([]string, error) {
	prefix := c.prefix()
	it := c.store.db.NewIterator(prefix, nil)
	defer it.Release()
	var keys []string
	for it.Next() {
		keys = append(keys, string(bytes.TrimPrefix(it.Key(), prefix)))
	}
	return keys, it.Error()
}

type kvDocument struct {
	collection *kvCollection
	key        string
}

func (d *kvDocument) dbKey() []byte {
	return append(d.collection.prefix(), d.key...)
}

func (d *kvDocument) Key() string { return d.key }

func (d *kvDocument) Exists() (bool, error) {
	return d.collection.store.db.Has(d.dbKey())
}

func (d *kvDocument) Fields() (map[string]any, error) {
	return d.read()
}

func (d *kvDocument) read() (map[string]any, error) {
	db := d.collection.store.db
	key := d.dbKey()
	ok, err := db.Has(key)
	if err != nil || !ok {
		return map[string]any{}, err
	}
	raw, err := db.Get(key)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	fields := map[string]any{}
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("document %q: %w", d.key, err)
	}
	return fields, nil
}

func (d *kvDocument) Update(fields map[string]any) error {
	store := d.collection.store
	store.mu.Lock()
	defer store.mu.Unlock()
	current, err := d.read()
	if err != nil {
		return err
	}
	maps.Copy(current, fields)
	raw, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("document %q: %w", d.key, err)
	}
	return store.db.Put(d.dbKey(), raw)
}

func (d *kvDocument) GetSubCollection(name string) Collection {
	path := append(slices.Clone(d.collection.path), d.key, name)
	return &kvCollection{store: d.collection.store, path: path}
}
