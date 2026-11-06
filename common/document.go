package common

import (
	"maps"
	"slices"
	"sync"
)

// Document is a key-value document (the Python Document, a MongoDB document
// in the original). The State, the ContractExecution and the navigators only
// depend on this interface, so the storage backend can be swapped.
//
// Values are JSON-like: string, bool, numbers (any Go integer or float type,
// or json.Number), []any, map[string]any and nil. A persistent backend may
// return numbers as json.Number.
type Document interface {
	// Key returns the document's key in its collection.
	Key() string
	// Exists reports whether the document has been written.
	Exists() (bool, error)
	// Fields returns a copy of the document's fields; empty if it does not exist.
	Fields() (map[string]any, error)
	// Update merges fields into the document, creating it if needed (Mongo $set).
	Update(fields map[string]any) error
	// GetSubCollection returns the named nested collection. Like in Python,
	// it is created on first use.
	GetSubCollection(name string) Collection
}

// Collection is a mapping from key to Document (the Python Collection).
type Collection interface {
	// Document returns the document at key, whether or not it exists yet.
	Document(key string) Document
	// Has reports whether a document exists at key ("key in collection").
	Has(key string) (bool, error)
	// Keys returns the keys of all existing documents, sorted.
	Keys() ([]string, error)
	// Delete removes the document at key, if it exists.
	Delete(key string) error
}

// MemoryDocument is an in-memory Document, for tests and development.
type MemoryDocument struct {
	mu          sync.RWMutex
	key         string
	exists      bool
	fields      map[string]any
	collections map[string]*MemoryCollection
}

// NewMemoryDocument returns an existing MemoryDocument holding a shallow copy of fields.
func NewMemoryDocument(fields map[string]any) *MemoryDocument {
	d := newMemoryDocument("")
	d.exists = true
	maps.Copy(d.fields, fields)
	return d
}

func newMemoryDocument(key string) *MemoryDocument {
	return &MemoryDocument{key: key, fields: map[string]any{}, collections: map[string]*MemoryCollection{}}
}

func (d *MemoryDocument) Key() string { return d.key }

func (d *MemoryDocument) Exists() (bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.exists, nil
}

func (d *MemoryDocument) Fields() (map[string]any, error) {
	return d.Snapshot(), nil
}

func (d *MemoryDocument) Update(fields map[string]any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	maps.Copy(d.fields, fields)
	d.exists = true
	return nil
}

func (d *MemoryDocument) GetSubCollection(name string) Collection {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.collections[name]
	if !ok {
		c = NewMemoryCollection()
		d.collections[name] = c
	}
	return c
}

// Get returns one field; a convenience for tests.
func (d *MemoryDocument) Get(key string) (any, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.fields[key]
	return v, ok
}

// Snapshot returns a shallow copy of the document's fields.
func (d *MemoryDocument) Snapshot() map[string]any {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return maps.Clone(d.fields)
}

// MemoryCollection is an in-memory Collection, for tests and development.
type MemoryCollection struct {
	mu   sync.Mutex
	docs map[string]*MemoryDocument
}

// NewMemoryCollection returns an empty MemoryCollection.
func NewMemoryCollection() *MemoryCollection {
	return &MemoryCollection{docs: map[string]*MemoryDocument{}}
}

func (c *MemoryCollection) Document(key string) Document {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.docs[key]
	if !ok {
		d = newMemoryDocument(key)
		c.docs[key] = d
	}
	return d
}

func (c *MemoryCollection) Has(key string) (bool, error) {
	c.mu.Lock()
	d, ok := c.docs[key]
	c.mu.Unlock()
	if !ok {
		return false, nil
	}
	return d.Exists()
}

func (c *MemoryCollection) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.docs, key)
	return nil
}

func (c *MemoryCollection) Keys() ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var keys []string
	for key, d := range c.docs {
		if ok, _ := d.Exists(); ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys, nil
}
