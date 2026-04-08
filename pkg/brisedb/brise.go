package brisedb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/birand/brisedb/pkg/volumeserver"
)

// FsyncMode controls when the WAL flusher calls fsync after writing.
type FsyncMode int

const (
	// FsyncEverySec syncs the WAL to disk at most once per second (default).
	// A crash can lose at most ~1 second of committed writes.
	FsyncEverySec FsyncMode = iota

	// FsyncAlways syncs the WAL after every batch of writes.
	// Safest: zero data loss on crash; slowest under high write load.
	FsyncAlways

	// FsyncNo never calls fsync. The OS flushes at its own discretion.
	// Fastest; a crash can lose any unflushed writes.
	FsyncNo
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

// walMsg is a request sent to the WAL flusher goroutine.
// Normal writes set data; Compact swaps set newFile (data is nil).
type walMsg struct {
	data    []byte   // serialised WAL line (including trailing newline)
	newFile *os.File // non-nil: swap the active WAL file after flushing pending data
	done    chan error
}

// BriseDB encapsulates shared database state. Thread-safe.
// Transaction state lives in Session — one per connection.
type BriseDB struct {
	mu          keyedMutex                     // sharded per-key RWMutex (64 shards)
	hindex      *hashIndex                     // persistent on-disk hash index (key → NeedleAddr)
	expiry      [numShards]map[string]time.Time // TTL cache; shard i protected by mu.shards[i]
	pubsub      *PubSub
	replication *replicationManager
	volumes     *VolumePool
	walFile     *os.File
	walPath     string
	walCh       chan walMsg    // WAL write requests; consumed by walFlusher goroutine
	walDone     chan struct{}  // closed by walFlusher after it drains and exits
	stopCh      chan struct{}
	fsyncMode   FsyncMode
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

	// CacheSize is the maximum number of bytes to hold in the in-memory
	// LRU read cache. 0 disables caching (default).
	// Example: 256 * 1024 * 1024 for a 256 MiB cache.
	CacheSize uint64

	// Fsync controls when the WAL flusher calls fsync after writing.
	// Default (zero value) is FsyncEverySec.
	Fsync FsyncMode
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

	volumes, err := newVolumePool(localVM, remotes, dataDir, opt.ReplicationFactor, opt.CacheSize)
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
		pubsub:      newPubSub(),
		replication: newReplicationManager(),
		volumes:     volumes,
		walFile:     walFile,
		walPath:     walPath,
		walCh:       make(chan walMsg, 1024),
		walDone:     make(chan struct{}),
		stopCh:      make(chan struct{}),
		fsyncMode:   opt.Fsync,
	}
	for i := range db.expiry {
		db.expiry[i] = make(map[string]time.Time)
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
	go db.walFlusher()

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
	<-db.walDone // wait for walFlusher to drain remaining entries and exit
	db.volumes.Close()
	db.hindex.Close()
	return db.walFile.Close()
}

// rebuildExpiry scans the hash index and repopulates db.expiry for all
// keys that have a TTL.  Called on startup when the hash index already exists.
func (db *BriseDB) rebuildExpiry() error {
	return db.hindex.ForEach(time.Now(), func(key string, _ NeedleAddr, expiresAt int64) bool {
		if expiresAt > 0 {
			db.expiry[shardIndex(key)][key] = time.Unix(0, expiresAt)
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
	db.mu.LockAll()
	defer db.mu.UnlockAll()
	for i := range db.expiry {
		for key, exp := range db.expiry[i] {
			if now.After(exp) {
				db.hindex.Delete(key) //nolint:errcheck
				delete(db.expiry[i], key)
			}
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
	db.mu.LockAll()
	defer db.mu.UnlockAll()

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
		w.Write(data)         //nolint:errcheck
		w.WriteByte('\n')     //nolint:errcheck
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

	newWAL, err := os.OpenFile(db.walPath, os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("reopen WAL after compact: %w", err)
	}

	// Hand the new file to the flusher goroutine. It will flush any buffered
	// data to the old file first, then switch to newWAL atomically.
	done := make(chan error, 1)
	db.walCh <- walMsg{newFile: newWAL, done: done}
	if err := <-done; err != nil {
		return fmt.Errorf("swap WAL file: %w", err)
	}
	db.walFile.Close()
	db.walFile = newWAL
	return nil
}

// writeWAL enqueues a WAL entry for the flusher goroutine and waits until
// the entry has been written to disk. Value is stripped before writing.
func (db *BriseDB) writeWAL(op walEntry) error {
	op.Value = "" // never persist value in WAL
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("marshal WAL entry: %w", err)
	}
	done := make(chan error, 1)
	db.walCh <- walMsg{data: data, done: done} // flusher appends the newline
	if err := <-done; err != nil {
		return fmt.Errorf("write WAL: %w", err)
	}
	return nil
}

// walFlusher is the sole goroutine that writes to the WAL file.
// It batches concurrent requests and flushes them in a single syscall,
// then optionally calls fsync according to db.fsyncMode:
//
//   - FsyncAlways:   fsync after every batch (safest, slowest)
//   - FsyncEverySec: fsync at most once per second (default)
//   - FsyncNo:       never fsync (fastest, OS decides when to flush)
func (db *BriseDB) walFlusher() {
	defer close(db.walDone)

	// currentFile tracks which *os.File backs bw so we can call Sync on it.
	currentFile := db.walFile
	bw := bufio.NewWriterSize(currentFile, 64<<10)

	// pendingSync is set to true whenever data has been flushed to the OS
	// buffer but not yet fsync'd. Only used for FsyncEverySec.
	pendingSync := false

	// syncTicker fires once per second for FsyncEverySec mode.
	// For other modes we still create the ticker but never act on it, so
	// the select cases are compiled-away-equivalent (no goroutine overhead
	// beyond the ticker itself, which is negligible).
	syncTicker := time.NewTicker(time.Second)
	defer syncTicker.Stop()

	// doSync calls fsync on currentFile and clears pendingSync.
	doSync := func() error {
		err := currentFile.Sync()
		pendingSync = false
		return err
	}

	flushBatch := func(batch []walMsg) error {
		var writeErr error
		for _, m := range batch {
			if len(m.data) > 0 {
				if _, err := bw.Write(m.data); err != nil && writeErr == nil {
					writeErr = err
				} else if err == nil {
					bw.WriteByte('\n') //nolint:errcheck
				}
			}
		}
		if err := bw.Flush(); err != nil && writeErr == nil {
			writeErr = err
		}
		if writeErr != nil {
			return writeErr
		}

		switch db.fsyncMode {
		case FsyncAlways:
			writeErr = doSync()
		case FsyncEverySec:
			pendingSync = true
		// FsyncNo: do nothing
		}
		return writeErr
	}

	handleSpecial := func(msg walMsg) {
		if msg.newFile != nil {
			// File-swap from Compact: flush + sync the old file so the new
			// compacted WAL is durable before we start appending to it.
			err := bw.Flush()
			if err == nil && db.fsyncMode != FsyncNo {
				err = doSync()
			}
			msg.done <- err
			currentFile = msg.newFile
			bw = bufio.NewWriterSize(currentFile, 64<<10)
		} else {
			// Zero-data barrier message: flush + sync so the caller can rely
			// on durability (used by tests and explicit Sync() calls).
			err := bw.Flush()
			if err == nil && db.fsyncMode != FsyncNo {
				err = doSync()
			}
			msg.done <- err
		}
	}

	for {
		var batch []walMsg

		// Block until at least one message, a sync tick, or shutdown.
		select {
		case msg := <-db.walCh:
			if msg.newFile != nil || len(msg.data) == 0 {
				handleSpecial(msg)
				continue
			}
			batch = append(batch, msg)
		case <-syncTicker.C:
			if pendingSync {
				doSync() //nolint:errcheck — best-effort periodic sync
			}
			continue
		case <-db.stopCh:
			// Drain remaining messages so callers are not left blocking.
			for {
				select {
				case msg := <-db.walCh:
					if msg.newFile != nil || len(msg.data) == 0 {
						handleSpecial(msg)
					} else {
						batch = append(batch, msg)
					}
				default:
					err := flushBatch(batch)
					for _, m := range batch {
						m.done <- err
					}
					// Final fsync on shutdown regardless of mode.
					if err == nil {
						currentFile.Sync() //nolint:errcheck
					}
					return
				}
			}
		}

		// Drain any additional messages already queued (non-blocking).
	drain:
		for len(batch) < 512 {
			select {
			case msg := <-db.walCh:
				if msg.newFile != nil || len(msg.data) == 0 {
					// Flush current batch first, then handle the special msg.
					err := flushBatch(batch)
					for _, m := range batch {
						m.done <- err
					}
					batch = nil
					handleSpecial(msg)
					break drain
				}
				batch = append(batch, msg)
			default:
				break drain
			}
		}

		if len(batch) == 0 {
			continue
		}
		err := flushBatch(batch)
		for _, m := range batch {
			m.done <- err
		}
	}
}

// applyEntry applies a single WAL entry to the hash index and expiry cache.
// Must be called with mu held (or during single-threaded replay).
func (db *BriseDB) applyEntry(op walEntry) {
	si := shardIndex(op.Key)
	switch op.Type {
	case "SET":
		addr := NeedleAddr{VolumeID: op.VolumeID, Offset: op.Offset, Size: op.Size}
		db.hindex.Set(op.Key, addr, op.ExpiresAt) //nolint:errcheck
		if op.ExpiresAt > 0 {
			db.expiry[si][op.Key] = time.Unix(0, op.ExpiresAt)
		} else {
			delete(db.expiry[si], op.Key)
		}
		db.volumes.EnsureVolume(op.VolumeID) //nolint:errcheck
	case "DELETE":
		db.hindex.Delete(op.Key) //nolint:errcheck
		delete(db.expiry[si], op.Key)
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

	db.mu.Lock(op.Key)
	db.applyEntry(op)
	db.mu.Unlock(op.Key)
	return db.writeWAL(op)
}

// Replication returns the replication manager (used by the server handler).
func (db *BriseDB) Replication() *replicationManager { return db.replication }

// VolumeStats returns info about all open volume files across all drives.
func (db *BriseDB) VolumeStats() []VolumeInfo { return db.volumes.Stats() }

// CacheStats returns LRU read-cache metrics.
// All fields are zero if the cache is disabled (CacheSize == 0).
func (db *BriseDB) CacheStats() LRUStats { return db.volumes.CacheStats() }

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
