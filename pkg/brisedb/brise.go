package brisedb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	ExpiresAt int64  `json:",omitempty"` // Unix seconds; 0 = no expiry
}

// BriseDB encapsulates shared database state. Thread-safe.
// Transaction state lives in Session — one per connection.
type BriseDB struct {
	mu          sync.RWMutex
	index       map[string]NeedleAddr // key → location in a volume file
	expiry      map[string]time.Time  // keys with a TTL
	pubsub      *PubSub
	replication *replicationManager
	volumes     *VolumeManager
	walFile     *os.File
	walPath     string
	stopCh      chan struct{}
}

// NewBriseDB opens (or creates) the database at dataDir.
// The WAL is stored at dataDir/wal.log; volume files at dataDir/volumes/.
func NewBriseDB(dataDir string) (*BriseDB, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	walPath := filepath.Join(dataDir, "wal.log")
	walFile, err := os.OpenFile(walPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open WAL: %w", err)
	}

	volDir := filepath.Join(dataDir, "volumes")
	volumes, err := newVolumeManager(volDir)
	if err != nil {
		walFile.Close()
		return nil, fmt.Errorf("open volumes: %w", err)
	}

	db := &BriseDB{
		index:       make(map[string]NeedleAddr),
		expiry:      make(map[string]time.Time),
		pubsub:      newPubSub(),
		replication: newReplicationManager(),
		volumes:     volumes,
		walFile:     walFile,
		walPath:     walPath,
		stopCh:      make(chan struct{}),
	}

	if err := db.replayWAL(); err != nil {
		walFile.Close()
		volumes.Close()
		return nil, fmt.Errorf("replay WAL: %w", err)
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
	return db.walFile.Close()
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
			delete(db.index, key)
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

// Compact rewrites the WAL with only live keys, discarding history and
// already-expired entries. Volume files are not compacted (Phase 1).
func (db *BriseDB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	f, err := os.OpenFile(db.walPath, os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("truncate WAL: %w", err)
	}
	w := bufio.NewWriter(f)
	for key, addr := range db.index {
		if exp, ok := db.expiry[key]; ok && now.After(exp) {
			continue
		}
		op := walEntry{
			Type:     "SET",
			Key:      key,
			VolumeID: addr.VolumeID,
			Offset:   addr.Offset,
			Size:     addr.Size,
		}
		if exp, ok := db.expiry[key]; ok {
			op.ExpiresAt = exp.Unix()
		}
		data, err := json.Marshal(op)
		if err != nil {
			f.Close()
			return fmt.Errorf("marshal WAL entry: %w", err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			f.Close()
			return fmt.Errorf("write WAL entry: %w", err)
		}
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

// applyEntry applies a single WAL entry to the in-memory index.
// Must be called with mu held (or during single-threaded replay).
func (db *BriseDB) applyEntry(op walEntry) {
	switch op.Type {
	case "SET":
		db.index[op.Key] = NeedleAddr{
			VolumeID: op.VolumeID,
			Offset:   op.Offset,
			Size:     op.Size,
		}
		if op.ExpiresAt > 0 {
			db.expiry[op.Key] = time.Unix(op.ExpiresAt, 0)
		} else {
			delete(db.expiry, op.Key)
		}
		// Ensure volume is registered for subsequent reads
		db.volumes.EnsureVolume(op.VolumeID) //nolint:errcheck
	case "DELETE":
		delete(db.index, op.Key)
		delete(db.expiry, op.Key)
	}
}

// Snapshot returns all live keys as JSON walEntry lines (with Value populated)
// for sending to a new replica. Reads value bytes from volume files.
func (db *BriseDB) Snapshot() [][]byte {
	now := time.Now()

	// Collect index under read lock
	type snap struct {
		key       string
		addr      NeedleAddr
		expiresAt int64
	}
	db.mu.RLock()
	snaps := make([]snap, 0, len(db.index))
	for k, addr := range db.index {
		if exp, ok := db.expiry[k]; ok {
			if now.After(exp) {
				continue
			}
			snaps = append(snaps, snap{k, addr, exp.Unix()})
		} else {
			snaps = append(snaps, snap{k, addr, 0})
		}
	}
	db.mu.RUnlock()

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
	db.applyEntry(op)
	err := db.writeWAL(op)
	db.mu.Unlock()
	return err
}

// Replication returns the replication manager (used by the server handler).
func (db *BriseDB) Replication() *replicationManager { return db.replication }

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
