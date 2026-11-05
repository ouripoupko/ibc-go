package execution

import (
	"slices"
	"sync"
)

// MemoryLedger is an in-memory Ledger, for tests and development.
type MemoryLedger struct {
	mu      sync.Mutex
	records []Record
}

func (l *MemoryLedger) Log(record Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, record)
	return nil
}

// All returns the logged records in order.
func (l *MemoryLedger) All() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.records)
}

// Records calls fn for every record with index >= from (1-based), in order.
func (l *MemoryLedger) Records(from uint64, fn func(uint64, Record) error) error {
	if from == 0 {
		from = 1
	}
	for i, r := range l.All() {
		if index := uint64(i + 1); index >= from {
			if err := fn(index, r); err != nil {
				return err
			}
		}
	}
	return nil
}
