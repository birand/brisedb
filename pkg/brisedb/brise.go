package brisedb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

/* Map string:string */
type Map = map[string]string

// walEntry is the on-disk format for WAL records.
type walEntry struct {
	Type      string
	Key       string
	Value     string `json:",omitempty"`
	ExpiresAt int64  `json:",omitempty"` // Unix seconds; 0 = no expiry
}

// BriseDB encapsulates the shared database state (store, WAL). Thread-safe.
// Transaction state lives in Session — one per connection/client.
type BriseDB struct {
	mu          sync.RWMutex
	store       map[string]string
	valueCounts map[string]int
	expiry      map[string]time.Time // keys with a TTL
	pubsub      *PubSub
	replication *replicationManager
	walFile     *os.File
	walPath     string
	stopCh      chan struct{}
}

// NewBriseDB creates a new BriseDB instance. walPath is the path to the WAL file.
func NewBriseDB(walPath string) (*BriseDB, error) {
	walFile, err := os.OpenFile(walPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open WAL file: %w", err)
	}

	db := &BriseDB{
		store:       make(map[string]string),
		valueCounts: make(map[string]int),
		expiry:      make(map[string]time.Time),
		pubsub:      newPubSub(),
		replication: newReplicationManager(),
		walFile:     walFile,
		walPath:     walPath,
		stopCh:      make(chan struct{}),
	}

	if err := db.replayWAL(); err != nil {
		return nil, fmt.Errorf("failed to replay WAL: %w", err)
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

// Close stops the eviction goroutine and closes the WAL file.
func (db *BriseDB) Close() error {
	close(db.stopCh)
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

// evictExpired deletes all keys whose TTL has elapsed.
func (db *BriseDB) evictExpired() {
	now := time.Now()
	db.mu.Lock()
	defer db.mu.Unlock()
	for key, exp := range db.expiry {
		if now.After(exp) {
			if oldVal, ok := db.store[key]; ok {
				db.valueCounts[oldVal]--
				delete(db.store, key)
			}
			delete(db.expiry, key)
		}
	}
}

// replayWAL replays the operations from the WAL file.
// Entries whose ExpiresAt is in the past are skipped.
func (db *BriseDB) replayWAL() error {
	if _, err := db.walFile.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek to the beginning of the WAL file: %w", err)
	}

	now := time.Now()
	scanner := bufio.NewScanner(db.walFile)
	for scanner.Scan() {
		var op walEntry
		if err := json.Unmarshal(scanner.Bytes(), &op); err != nil {
			return fmt.Errorf("failed to unmarshal WAL entry: %w", err)
		}

		// Skip SET entries that have already expired
		if op.Type == "SET" && op.ExpiresAt > 0 && time.Unix(op.ExpiresAt, 0).Before(now) {
			continue
		}
		db.applyEntry(op)
	}

	return scanner.Err()
}

// Compact rewrites the WAL with only the current committed state, discarding
// history and already-expired keys.
func (db *BriseDB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	f, err := os.OpenFile(db.walPath, os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to truncate WAL: %w", err)
	}

	w := bufio.NewWriter(f)
	for key, value := range db.store {
		// Skip keys that have expired but not yet been evicted
		if exp, ok := db.expiry[key]; ok && now.After(exp) {
			continue
		}
		op := walEntry{Type: "SET", Key: key, Value: value}
		if exp, ok := db.expiry[key]; ok {
			op.ExpiresAt = exp.Unix()
		}
		data, err := json.Marshal(op)
		if err != nil {
			f.Close()
			return fmt.Errorf("failed to marshal WAL entry: %w", err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			f.Close()
			return fmt.Errorf("failed to write WAL entry: %w", err)
		}
	}

	if err := w.Flush(); err != nil {
		f.Close()
		return fmt.Errorf("failed to flush WAL: %w", err)
	}
	f.Close()

	// Reopen in append mode so subsequent writes work correctly
	db.walFile.Close()
	db.walFile, err = os.OpenFile(db.walPath, os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to reopen WAL after compact: %w", err)
	}

	return nil
}

// writeWAL appends a walEntry to the WAL file and fans it out to replicas.
// Must be called with mu held.
func (db *BriseDB) writeWAL(op walEntry) error {
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("failed to marshal WAL entry: %w", err)
	}
	line := append(data, '\n')
	if _, err := db.walFile.Write(line); err != nil {
		return fmt.Errorf("failed to write to WAL file: %w", err)
	}
	db.replication.fanout(data) // non-blocking; slow replicas drop entries
	return nil
}

// applyEntry applies a single WAL entry to the in-memory store.
// Must be called with mu held.
func (db *BriseDB) applyEntry(op walEntry) {
	switch op.Type {
	case "SET":
		if oldValue, ok := db.store[op.Key]; ok {
			db.valueCounts[oldValue]--
		}
		db.store[op.Key] = op.Value
		db.valueCounts[op.Value]++
		if op.ExpiresAt > 0 {
			db.expiry[op.Key] = time.Unix(op.ExpiresAt, 0)
		} else {
			delete(db.expiry, op.Key)
		}
	case "DELETE":
		if oldValue, ok := db.store[op.Key]; ok {
			db.valueCounts[oldValue]--
		}
		delete(db.store, op.Key)
		delete(db.expiry, op.Key)
	}
}

// Snapshot returns all current WAL entries as JSON lines (without newlines).
// It is called by the replication handler to send an initial state to a new replica.
func (db *BriseDB) Snapshot() [][]byte {
	now := time.Now()
	db.mu.RLock()
	defer db.mu.RUnlock()

	entries := make([][]byte, 0, len(db.store))
	for key, value := range db.store {
		if exp, ok := db.expiry[key]; ok && now.After(exp) {
			continue
		}
		op := walEntry{Type: "SET", Key: key, Value: value}
		if exp, ok := db.expiry[key]; ok {
			op.ExpiresAt = exp.Unix()
		}
		data, _ := json.Marshal(op)
		entries = append(entries, data)
	}
	return entries
}

// ApplyReplicationEntry decodes a WAL JSON line and applies it to the store.
// Used by replica instances to consume entries streamed from the primary.
func (db *BriseDB) ApplyReplicationEntry(data []byte) error {
	var op walEntry
	if err := json.Unmarshal(data, &op); err != nil {
		return fmt.Errorf("replication: bad entry: %w", err)
	}
	db.mu.Lock()
	db.applyEntry(op)
	db.mu.Unlock()
	return nil
}

// Replication returns the replication manager (used by the server handler).
func (db *BriseDB) Replication() *replicationManager { return db.replication }

// Transaction points to a key:value storage
type Transaction struct {
	store   map[string]string
	deleted map[string]bool
	// expiry tracks TTL changes within this transaction:
	//   zero time  → clear expiry on commit (Set / Persist)
	//   non-zero   → set expiry on commit (SetEX)
	//   key absent → no change to expiry on commit
	expiry map[string]time.Time
	next   *Transaction
}

// TransactionStack maintains a list of active/suspended transactions
type TransactionStack struct {
	top  *Transaction
	size int
}

// PushTransaction creates a new active transaction
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

// PopTransaction deletes a transaction from the stack
func (ts *TransactionStack) PopTransaction() error {
	if ts.top == nil {
		return fmt.Errorf("ERROR: No Active Transactions")
	}
	ts.top = ts.top.next
	ts.size--
	return nil
}

// Peek returns the active transaction
func (ts *TransactionStack) Peek() *Transaction {
	return ts.top
}
