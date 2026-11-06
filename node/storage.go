package node

import (
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// storageCache is how long a measured data folder size is reused, so that
// frequent node_info calls do not walk the folder each time.
const storageCache = 30 * time.Second

// storage measures the size of the node's data folder: the database and the
// node key. Logs are not in it (the node writes them to standard error).
type storage struct {
	dir  string
	mu   sync.Mutex
	size int64
	at   time.Time
}

// Size returns the total size of the files in the data folder, in bytes.
// ok is false if the node has no data folder (an in-memory database).
func (s *storage) Size() (size int64, ok bool) {
	if s == nil || s.dir == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.at) < storageCache {
		return s.size, true
	}
	var total int64
	// Files can vanish while the database compacts; skip what cannot be read.
	_ = filepath.WalkDir(s.dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	s.size, s.at = total, time.Now()
	return total, true
}
