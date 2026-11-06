package cordial

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"slices"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"

	"github.com/ouripoupko/ibc-go/identity"
)

// Store persists a miner's state.
type Store interface {
	SaveConfig(cfg *Config) error
	LoadConfig() (*Config, bool, error)
	SaveBlock(b *Block) error
	Block(h ethcommon.Hash) (*Block, error)
	Blocks() ([]*Block, error)
	SaveMempool(id ethcommon.Hash, stx identity.SignedTx) error
	DeleteMempool(id ethcommon.Hash) error
	Mempool() (map[ethcommon.Hash]identity.SignedTx, []ethcommon.Hash, error)
	SaveIncluded(id ethcommon.Hash, stx identity.SignedTx) error
	DeleteIncluded(id ethcommon.Hash) error
	Included() (map[ethcommon.Hash]identity.SignedTx, error)
	// SaveOutput records, atomically, blocks and transactions output and the next index.
	SaveOutput(blocks, txs []ethcommon.Hash, next uint64) error
	OutputState() (blocks, txs []ethcommon.Hash, next uint64, err error)
	SaveWaiting(w *waitState) error
	LoadWaiting() (*waitState, error)
}

// KVStore keeps a miner's state in a key-value store under a prefix.
type KVStore struct {
	db     ethdb.KeyValueStore
	prefix []byte
}

// NewKVStore returns the store of one (agent, contract) miner.
func NewKVStore(db ethdb.KeyValueStore, agent ethcommon.Address, contract string) *KVStore {
	p := []byte("ibc/cm/")
	p = append(p, agent.Bytes()...)
	p = binary.AppendUvarint(p, uint64(len(contract)))
	p = append(p, contract...)
	return &KVStore{db: db, prefix: p}
}

// MinerPrefix is the key prefix of every miner's state; StoredMiners lists them.
var MinerPrefix = []byte("ibc/cm/")

// StoredMiners lists the (agent, contract) pairs that have a stored configuration.
func StoredMiners(db ethdb.KeyValueStore) ([]struct {
	Agent    ethcommon.Address
	Contract string
}, error) {
	type pair = struct {
		Agent    ethcommon.Address
		Contract string
	}
	var out []pair
	it := db.NewIterator(MinerPrefix, nil)
	defer it.Release()
	for it.Next() {
		rest := it.Key()[len(MinerPrefix):]
		if len(rest) < 20 {
			continue
		}
		agent := ethcommon.BytesToAddress(rest[:20])
		n, w := binary.Uvarint(rest[20:])
		if w <= 0 || len(rest) < 20+w+int(n) {
			continue
		}
		contract := string(rest[20+w : 20+w+int(n)])
		if string(rest[20+w+int(n):]) == "cfg" {
			out = append(out, pair{agent, contract})
		}
	}
	return out, it.Error()
}

func (s *KVStore) key(parts ...[]byte) []byte {
	return append(slices.Clone(s.prefix), bytes.Join(parts, nil)...)
}

func (s *KVStore) get(key []byte) ([]byte, bool, error) {
	ok, err := s.db.Has(key)
	if err != nil || !ok {
		return nil, false, err
	}
	v, err := s.db.Get(key)
	return v, err == nil, err
}

func (s *KVStore) SaveConfig(cfg *Config) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.db.Put(s.key([]byte("cfg")), raw)
}

func (s *KVStore) LoadConfig() (*Config, bool, error) {
	raw, ok, err := s.get(s.key([]byte("cfg")))
	if err != nil || !ok {
		return nil, false, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, false, err
	}
	return &cfg, true, nil
}

func (s *KVStore) SaveBlock(b *Block) error {
	h := b.Hash()
	return s.db.Put(s.key([]byte("b/"), h[:]), b.Encode())
}

func (s *KVStore) Block(h ethcommon.Hash) (*Block, error) {
	raw, ok, err := s.get(s.key([]byte("b/"), h[:]))
	if err != nil || !ok {
		return nil, err
	}
	return DecodeBlock(raw)
}

func (s *KVStore) Blocks() ([]*Block, error) {
	var out []*Block
	err := s.iterate([]byte("b/"), func(_, v []byte) error {
		b, err := DecodeBlock(v)
		if err != nil {
			return err
		}
		out = append(out, b)
		return nil
	})
	return out, err
}

func (s *KVStore) iterate(sub []byte, fn func(k, v []byte) error) error {
	prefix := s.key(sub)
	it := s.db.NewIterator(prefix, nil)
	defer it.Release()
	for it.Next() {
		if err := fn(it.Key()[len(prefix):], slices.Clone(it.Value())); err != nil {
			return err
		}
	}
	return it.Error()
}

// Mempool entries are keyed by an arrival counter, so the order survives restarts.
func (s *KVStore) SaveMempool(id ethcommon.Hash, stx identity.SignedTx) error {
	raw, err := json.Marshal(struct {
		ID ethcommon.Hash
		Tx identity.SignedTx
	}{id, stx})
	if err != nil {
		return err
	}
	seq, err := s.nextSeq()
	if err != nil {
		return err
	}
	batch := s.db.NewBatch()
	if err := batch.Put(s.key([]byte("mp/"), binary.BigEndian.AppendUint64(nil, seq)), raw); err != nil {
		return err
	}
	if err := batch.Put(s.key([]byte("mi/"), id[:]), binary.BigEndian.AppendUint64(nil, seq)); err != nil {
		return err
	}
	return batch.Write()
}

func (s *KVStore) nextSeq() (uint64, error) {
	k := s.key([]byte("seq"))
	raw, ok, err := s.get(k)
	if err != nil {
		return 0, err
	}
	var n uint64
	if ok && len(raw) == 8 {
		n = binary.BigEndian.Uint64(raw)
	}
	return n + 1, s.db.Put(k, binary.BigEndian.AppendUint64(nil, n+1))
}

func (s *KVStore) DeleteMempool(id ethcommon.Hash) error {
	seq, ok, err := s.get(s.key([]byte("mi/"), id[:]))
	if err != nil || !ok {
		return err
	}
	batch := s.db.NewBatch()
	if err := batch.Delete(s.key([]byte("mp/"), seq)); err != nil {
		return err
	}
	if err := batch.Delete(s.key([]byte("mi/"), id[:])); err != nil {
		return err
	}
	return batch.Write()
}

func (s *KVStore) Mempool() (map[ethcommon.Hash]identity.SignedTx, []ethcommon.Hash, error) {
	out := map[ethcommon.Hash]identity.SignedTx{}
	var order []ethcommon.Hash
	err := s.iterate([]byte("mp/"), func(_, v []byte) error {
		var e struct {
			ID ethcommon.Hash
			Tx identity.SignedTx
		}
		if err := json.Unmarshal(v, &e); err != nil {
			return err
		}
		out[e.ID] = e.Tx
		order = append(order, e.ID)
		return nil
	})
	return out, order, err
}

func (s *KVStore) SaveIncluded(id ethcommon.Hash, stx identity.SignedTx) error {
	raw, err := json.Marshal(stx)
	if err != nil {
		return err
	}
	return s.db.Put(s.key([]byte("in/"), id[:]), raw)
}

func (s *KVStore) DeleteIncluded(id ethcommon.Hash) error {
	return s.db.Delete(s.key([]byte("in/"), id[:]))
}

func (s *KVStore) Included() (map[ethcommon.Hash]identity.SignedTx, error) {
	out := map[ethcommon.Hash]identity.SignedTx{}
	err := s.iterate([]byte("in/"), func(k, v []byte) error {
		var stx identity.SignedTx
		if err := json.Unmarshal(v, &stx); err != nil {
			return err
		}
		out[ethcommon.BytesToHash(k)] = stx
		return nil
	})
	return out, err
}

func (s *KVStore) SaveOutput(blocks, txs []ethcommon.Hash, next uint64) error {
	batch := s.db.NewBatch()
	for _, h := range blocks {
		if err := batch.Put(s.key([]byte("ob/"), h[:]), []byte{1}); err != nil {
			return err
		}
	}
	for _, id := range txs {
		if err := batch.Put(s.key([]byte("ot/"), id[:]), []byte{1}); err != nil {
			return err
		}
	}
	if err := batch.Put(s.key([]byte("next")), binary.BigEndian.AppendUint64(nil, next)); err != nil {
		return err
	}
	return batch.Write()
}

func (s *KVStore) OutputState() ([]ethcommon.Hash, []ethcommon.Hash, uint64, error) {
	var blocks, txs []ethcommon.Hash
	if err := s.iterate([]byte("ob/"), func(k, _ []byte) error {
		blocks = append(blocks, ethcommon.BytesToHash(k))
		return nil
	}); err != nil {
		return nil, nil, 0, err
	}
	if err := s.iterate([]byte("ot/"), func(k, _ []byte) error {
		txs = append(txs, ethcommon.BytesToHash(k))
		return nil
	}); err != nil {
		return nil, nil, 0, err
	}
	raw, ok, err := s.get(s.key([]byte("next")))
	if err != nil {
		return nil, nil, 0, err
	}
	next := uint64(1)
	if ok && len(raw) == 8 {
		next = binary.BigEndian.Uint64(raw)
	}
	return blocks, txs, next, nil
}

type storedWait struct {
	Leader  []byte
	Changes []*change
}

func (s *KVStore) SaveWaiting(w *waitState) error {
	k := s.key([]byte("wait"))
	if w == nil {
		return s.db.Delete(k)
	}
	raw, err := json.Marshal(storedWait{Leader: w.Leader.Encode(), Changes: w.Changes})
	if err != nil {
		return err
	}
	return s.db.Put(k, raw)
}

func (s *KVStore) LoadWaiting() (*waitState, error) {
	raw, ok, err := s.get(s.key([]byte("wait")))
	if err != nil || !ok {
		return nil, err
	}
	var sw storedWait
	if err := json.Unmarshal(raw, &sw); err != nil {
		return nil, err
	}
	leader, err := DecodeBlock(sw.Leader)
	if err != nil {
		return nil, err
	}
	return &waitState{Leader: leader, Changes: sw.Changes}, nil
}
