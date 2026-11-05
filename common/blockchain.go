package common

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/ethdb"
)

// BlockChain is an agent's ledger (Python common/blockchain.py). Python kept
// one sequence per agent and filtered it by contract on read; here every
// contract has its own sequence, numbered from 1, because each contract is
// processed by its own queue.
type BlockChain struct {
	db    ethdb.KeyValueStore
	agent string

	mu        sync.Mutex
	contracts map[string]*ContractLedger
}

// NewBlockChain returns the ledger of agent stored in db.
func NewBlockChain(db ethdb.KeyValueStore, agent string) *BlockChain {
	return &BlockChain{db: db, agent: agent, contracts: map[string]*ContractLedger{}}
}

// Contract returns the ledger of one contract. It always returns the same
// instance for the same contract, so appends are serialised.
func (b *BlockChain) Contract(contract string) *ContractLedger {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.contracts[contract]
	if !ok {
		prefix := appendParts(slices.Clone(ledgerPrefix), b.agent, contract)
		l = &ContractLedger{db: b.db, prefix: prefix}
		b.contracts[contract] = l
	}
	return l
}

// ContractLedger is the ordered list of records of one contract.
type ContractLedger struct {
	db     ethdb.KeyValueStore
	prefix []byte
	mu     sync.Mutex
}

// Key layout: prefix + 'n' holds the length; prefix + 'r' + big-endian index
// holds a record, so records iterate in index order.
func (l *ContractLedger) lengthKey() []byte { return append(slices.Clone(l.prefix), 'n') }

func (l *ContractLedger) recordKey(index uint64) []byte {
	return binary.BigEndian.AppendUint64(append(slices.Clone(l.prefix), 'r'), index)
}

// Len returns the number of records.
func (l *ContractLedger) Len() (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.length()
}

func (l *ContractLedger) length() (uint64, error) {
	key := l.lengthKey()
	ok, err := l.db.Has(key)
	if err != nil || !ok {
		return 0, err
	}
	raw, err := l.db.Get(key)
	if err != nil {
		return 0, err
	}
	if len(raw) != 8 {
		return 0, fmt.Errorf("ledger: corrupt length")
	}
	return binary.BigEndian.Uint64(raw), nil
}

// ErrLedgerGap is returned by AppendAt when records before index are missing.
var ErrLedgerGap = errors.New("ledger: record index leaves a gap")

// Append adds a JSON record and returns its index (1 for the first record).
// The record and the new length are written in one atomic batch.
func (l *ContractLedger) Append(record json.RawMessage) (uint64, error) {
	index, _, err := l.AppendAt(0, record)
	return index, err
}

// AppendAt adds a record that must have the given index, which makes
// redelivery safe: a record whose index is already in the ledger is ignored
// and reported with appended == false. Index 0 means "the next index".
func (l *ContractLedger) AppendAt(index uint64, record json.RawMessage) (uint64, bool, error) {
	if !json.Valid(record) {
		return 0, false, fmt.Errorf("ledger: record is not valid JSON")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.length()
	if err != nil {
		return 0, false, err
	}
	switch {
	case index == 0:
		index = n + 1
	case index <= n:
		return index, false, nil
	case index > n+1:
		return 0, false, fmt.Errorf("%w: have %d records, got index %d", ErrLedgerGap, n, index)
	}
	if err := l.write(index, record); err != nil {
		return 0, false, err
	}
	return index, true, nil
}

func (l *ContractLedger) write(index uint64, record json.RawMessage) error {
	batch := l.db.NewBatch()
	if err := batch.Put(l.recordKey(index), record); err != nil {
		return err
	}
	if err := batch.Put(l.lengthKey(), binary.BigEndian.AppendUint64(nil, index)); err != nil {
		return err
	}
	return batch.Write()
}

// Get returns the record at index.
func (l *ContractLedger) Get(index uint64) (json.RawMessage, bool, error) {
	key := l.recordKey(index)
	ok, err := l.db.Has(key)
	if err != nil || !ok {
		return nil, false, err
	}
	raw, err := l.db.Get(key)
	return raw, err == nil, err
}

// Records calls fn for every record with index >= from, in order. It stops
// at the first error fn returns.
func (l *ContractLedger) Records(from uint64, fn func(index uint64, record json.RawMessage) error) error {
	if from == 0 {
		from = 1
	}
	recordPrefix := append(slices.Clone(l.prefix), 'r')
	start := binary.BigEndian.AppendUint64(nil, from)
	it := l.db.NewIterator(recordPrefix, start)
	defer it.Release()
	for it.Next() {
		index := binary.BigEndian.Uint64(it.Key()[len(recordPrefix):])
		if err := fn(index, slices.Clone(it.Value())); err != nil {
			return err
		}
	}
	return it.Error()
}
