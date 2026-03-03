package brisedb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/birand/brisedb/pkg/volumeserver"
)

/* Map string:string */
type Map = map[string]string

// walEntry is the on-disk format for WAL records.
// Value is never written to the WAL file; it is only populated when entries
// are sent over the replication stream (snapshot + live fanout).
type walEntry struct {
	Type      string
	Key       string
	Value     string `json:",omitempty"` // replication only
	VolumeID  uint32 `json:",omitempty"`
	Offset    uint64 `json:",omitempty"`
	Size      uint64 `json:",omitempty"`
	ExpiresAt int64  `json:",omitempty"` // Unix nanoseconds; 0 = no expiry
}

// BriseDB encapsulates shared database state. Thread-safe.
// Transaction state lives in Session — one per connection.
type BriseDB struct {
	mu          sync.RWMutex
	hindex      *hashIndex           // persistent on-disk hash index (key → NeedleAddr)
	expiry      map[string]time.Time // in-memory cache of TTL keys (subset of hindex)
	pubsub      *PubSub
	replication *replicationManager
	volumes     *VolumePool
	walFile     *os.File
	walPath     string
	stopCh      chan struct{}
}

// DBOptions configures optional behaviour of NewBriseDB.
type DBOptions struct {
	// ExtraDrives lists additional local directories for volume files.
	// The primary drive is always dataDir/volumes.
	ExtraDrives []string

	// MaxVolumeSize is the maximum size in bytes of a single volume file
	// before a new one is created. 0 means DefaultMaxVolumeSize (2 GiB).
	MaxVolumeSize uint64

	// VolumeServers is an optional list of remote HTTP volume server base URLs
	// (e.g. "http://10.0.0.2:8081"). When set, blob writes are distributed
	// across local drives and remote servers in round-robin order.
	VolumeServers []string

	// ReplicationFactor controls how many copies of each blob are stored
	// across the available backends. 1 = no replication (default).
	// Must be ≤ len(VolumeServers) + 1 (local counts as one backend).
	ReplicationFactor int
}

// NewBriseDB opens (or creates) the database at dataDir.
// The WAL is stored at dataDir/wal.log; volume files at dataDir/volumes/
// and any ExtraDrives specified in opts.
func NewBriseDB(dataDir string, opts ...DBOptions) (*BriseDB, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	var opt DBOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	walPath := filepath.Join(dataDir, "wal.log")
	walFile, err := os.OpenFile(walPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open WAL: %w", err)
	}

	drives := append([]string{filepath.Join(dataDir, "volumes")}, opt.ExtraDrives...)
	localVM, err := newVolumeManager(drives, opt.MaxVolumeSize)
	if err != nil {
		walFile.Close()
		return nil, fmt.Errorf("open volumes: %w", err)
	}

	// Build remote volume server clients.
	remotes := make([]*volumeserver.Client, 0, len(opt.VolumeServers))
	for _, url := range opt.VolumeServers {
		remotes = append(remotes, volumeserver.NewClient(url))
	}

	volumes, err := newVolumePool(localVM, remotes, dataDir, opt.ReplicationFactor)
	if err != nil {
		walFile.Close()
		localVM.Close()
		return nil, fmt.Errorf("open volume pool: %w", err)
	}

	idxPath := filepath.Join(dataDir, "index.hash")
	hindex, err := openHashIndex(idxPath, 0)
	if err != nil {
		walFile.Close()
		volumes.Close()
		return nil, fmt.Errorf("open hash index: %w", err)
	}

	db := &BriseDB{
		hindex:      hindex,
		expiry:      make(map[string]time.Time),
		pubsub:      newPubSub(),
		replication: newReplicationManager(),
		volumes:     volumes,
		walFile:     walFile,
		walPath:     walPath,
		stopCh:      make(chan struct{}),
	}

	// On first open (empty hash index), replay the WAL to rebuild it.
	// On subsequent opens the hash index is already up-to-date.
	if hindex.entryCount == 0 {
		if err := db.replayWAL(); err != nil {
			walFile.Close()
			volumes.Close()
			hindex.Close()
			return nil, fmt.Errorf("replay WAL: %w", err)
		}
	} else {
		// Rebuild the in-memory expiry cache from the hash index.
		if err := db.rebuildExpiry(); err != nil {
			walFile.Close()
			volumes.Close()
			hindex.Close()
			return nil, fmt.Errorf("rebuild expiry cache: %w", err)
		}
	}

	go db.evictionLoop()

	return db, nil
}

// PubSub returns the shared pub/sub manager.
func (db *BriseDB) PubSub() *PubSub { return db.pubsub }

// NewSession creates a new Session backed by this database.
func (db *BriseDB) NewSession() *Session {
	return &Session{
		db:           db,
		transactions: &TransactionStack{},
	}
}

// Close stops background goroutines and closes all files.
func (db *BriseDB) Close() error {
	close(db.stopCh)
	db.volumes.Close()
	db.hindex.Close()
	return db.walFile.Close()
}

// rebuildExpiry scans the hash index and repopulates db.expiry for all
// keys that have a TTL.  Called on startup when the hash index already exists.
func (db *BriseDB) rebuildExpiry() error {
	return db.hindex.ForEach(time.Now(), func(key string, _ NeedleAddr, expiresAt int64) bool {
		if expiresAt > 0 {
			db.expiry[key] = time.Unix(0, expiresAt)
		}
		return true
	})
}

// evictionLoop runs in the background and removes expired keys every second.
func (db *BriseDB) evictionLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			db.evictExpired()
		case <-db.stopCh:
			return
		}
	}
}

func (db *BriseDB) evictExpired() {
	now := time.Now()
	db.mu.Lock()
	defer db.mu.Unlock()
	for key, exp := range db.expiry {
		if now.After(exp) {
			db.hindex.Delete(key) //nolint:errcheck
			delete(db.expiry, key)
		}
	}
}

// replayWAL rebuilds the in-memory index from the WAL file.
// Expired SET entries are skipped.
func (db *BriseDB) replayWAL() error {
	if _, err := db.walFile.Seek(0, 0); err != nil {
		return fmt.Errorf("seek WAL: %w", err)
	}
	now := time.Now()
	scanner := bufio.NewScanner(db.walFile)
	for scanner.Scan() {
		var op walEntry
		if err := json.Unmarshal(scanner.Bytes(), &op); err != nil {
			return fmt.Errorf("unmarshal WAL entry: %w", err)
		}
		if op.Type == "SET" && op.ExpiresAt > 0 && time.Unix(op.ExpiresAt, 0).Before(now) {
			continue
		}
		db.applyEntry(op)
	}
	return scanner.Err()
}

// Compact rewrites both the WAL and the hash index, discarding deleted and
// expired entries. Volume files are not compacted (Phase 2 concern).
func (db *BriseDB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()

	// Compact the hash index first — this is the authoritative index.
	if err := db.hindex.Compact(now); err != nil {
		return fmt.Errorf("compact hash index: %w", err)
	}

	// Rewrite the WAL to match the compacted index (for crash recovery).
	f, err := os.OpenFile(db.walPath, os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("truncate WAL: %w", err)
	}
	w := bufio.NewWriter(f)
	walErr := db.hindex.ForEach(now, func(key string, addr NeedleAddr, expiresAt int64) bool {
		op := walEntry{
			Type:      "SET",
			Key:       key,
			VolumeID:  addr.VolumeID,
			Offset:    addr.Offset,
			Size:      addr.Size,
			ExpiresAt: expiresAt,
		}
		data, _ := json.Marshal(op)
		w.Write(append(data, '\n')) //nolint:errcheck
		return true
	})
	if walErr != nil {
		f.Close()
		return fmt.Errorf("compact WAL write: %w", walErr)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return fmt.Errorf("flush WAL: %w", err)
	}
	f.Close()

	db.walFile.Close()
	db.walFile, err = os.OpenFile(db.walPath, os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("reopen WAL after compact: %w", err)
	}
	return nil
}

// writeWAL appends a walEntry to the WAL file.
// Value is stripped before writing — the WAL only stores needle addresses.
// Must be called with mu held.
func (db *BriseDB) writeWAL(op walEntry) error {
	op.Value = "" // never persist value in WAL
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("marshal WAL entry: %w", err)
	}
	if _, err := db.walFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write WAL: %w", err)
	}
	return nil
}

// applyEntry applies a single WAL entry to the hash index and expiry cache.
// Must be called with mu held (or during single-threaded replay).
func (db *BriseDB) applyEntry(op walEntry) {
	switch op.Type {
	case "SET":
		addr := NeedleAddr{VolumeID: op.VolumeID, Offset: op.Offset, Size: op.Size}
		db.hindex.Set(op.Key, addr, op.ExpiresAt) //nolint:errcheck
		if op.ExpiresAt > 0 {
			db.expiry[op.Key] = time.Unix(op.ExpiresAt, 0)
		} else {
			delete(db.expiry, op.Key)
		}
		db.volumes.EnsureVolume(op.VolumeID) //nolint:errcheck
	case "DELETE":
		db.hindex.Delete(op.Key) //nolint:errcheck
		delete(db.expiry, op.Key)
	}
}

// Snapshot returns all live keys as JSON walEntry lines (with Value populated)
// for sending to a new replica. Reads value bytes from volume files.
func (db *BriseDB) Snapshot() [][]byte {
	now := time.Now()

	// Collect index via ForEach (no db.mu needed — hindex has its own lock).
	type snap struct {
		key       string
		addr      NeedleAddr
		expiresAt int64
	}
	var snaps []snap
	db.hindex.ForEach(now, func(key string, addr NeedleAddr, expiresAt int64) bool { //nolint:errcheck
		snaps = append(snaps, snap{key, addr, expiresAt})
		return true
	})

	// Read values from volumes (no lock held)
	result := make([][]byte, 0, len(snaps))
	for _, s := range snaps {
		data, err := db.volumes.Read(s.addr)
		if err != nil {
			continue
		}
		op := walEntry{
			Type:      "SET",
			Key:       s.key,
			Value:     string(data),
			ExpiresAt: s.expiresAt,
		}
		b, _ := json.Marshal(op)
		result = append(result, b)
	}
	return result
}

// ApplyReplicationEntry decodes a replication entry (which carries Value),
// writes the value to the local volume, updates the index, and writes the WAL.
func (db *BriseDB) ApplyReplicationEntry(data []byte) error {
	var op walEntry
	if err := json.Unmarshal(data, &op); err != nil {
		return fmt.Errorf("replication: bad entry: %w", err)
	}

	if op.Type == "SET" {
		addr, err := db.volumes.Write([]byte(op.Value))
		if err != nil {
			return fmt.Errorf("replication: write volume: %w", err)
		}
		op.VolumeID = addr.VolumeID
		op.Offset = addr.Offset
		op.Size = addr.Size
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	db.applyEntry(op)
	return db.writeWAL(op)
}

// Replication returns the replication manager (used by the server handler).
func (db *BriseDB) Replication() *replicationManager { return db.replication }

// VolumeStats returns info about all open volume files across all drives.
func (db *BriseDB) VolumeStats() []VolumeInfo { return db.volumes.Stats() }

// ------------------------------------------------------------------ //
// Transaction types
// ------------------------------------------------------------------ //

// Transaction holds in-flight key-value changes for one savepoint.
type Transaction struct {
	store   map[string]string
	deleted map[string]bool
	// expiry tracks TTL changes within this transaction:
	//   zero time  → clear expiry on commit (Set / Persist)
	//   non-zero   → set expiry on commit (SetEX)
	//   key absent → no change on commit
	expiry map[string]time.Time
	next   *Transaction
}

// TransactionStack is a linked-list stack of active transactions.
type TransactionStack struct {
	top  *Transaction
	size int
}

func (ts *TransactionStack) PushTransaction() {
	temp := Transaction{
		store:   make(Map),
		deleted: make(map[string]bool),
		expiry:  make(map[string]time.Time),
	}
	temp.next = ts.top
	ts.top = &temp
	ts.size++
}

func (ts *TransactionStack) PopTransaction() error {
	if ts.top == nil {
		return fmt.Errorf("ERROR: No Active Transactions")
	}
	ts.top = ts.top.next
	ts.size--
	return nil
}

func (ts *TransactionStack) Peek() *Transaction {
	return ts.top
}
