package brisedb

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// indexEntryBufPool pools []byte buffers used by encodeIndexEntry and
// readEntryAt to avoid per-call heap allocations on every read/write.
// A capacity of 256 bytes covers keys up to ~185 characters without
// falling back to a fresh allocation.
var indexEntryBufPool = sync.Pool{
	New: func() any { b := make([]byte, 0, 256); return &b },
}

// DefaultBucketCount is the initial hash table size (1M buckets = 8 MiB bucket table).
// At an average load factor of 1, this supports 1M keys before chains lengthen.
// Compact() rewrites the index with a larger bucket table when load > 2.
const DefaultBucketCount uint64 = 1 << 20 // 1 048 576

const (
	hiMagic      = "BRISEHDX"
	hiVersion    = uint32(1)
	hiHeaderSize = int64(64)
)

// hashIndex is a persistent open-addressing-style hash table stored in a
// single file.  Lookups require at most 2 random reads (bucket + entry) so
// the entire key space does not need to fit in RAM.
//
// File layout
//
//	[64 bytes]  header
//	[B × 8]     bucket table — B = bucket_count, each slot is a uint64 file
//	            offset of the chain head (0 = empty)
//	[…]         entry data region, append-only
//
// Entry on disk (variable length)
//
//	[8]  next_offset  — file offset of the previous entry with the same bucket
//	                    (forms an insertion-order chain, newest first); 0 = end
//	[2]  key_len
//	[N]  key bytes
//	[4]  VolumeID
//	[8]  Offset
//	[8]  Size
//	[8]  ExpiresAt    — Unix nanoseconds; 0 = no expiry
//	[1]  deleted      — 0 = alive, 1 = tombstone
//
// Writes prepend to the chain (update bucket → new entry → old head), so the
// newest entry is always reachable first.  ForEach does a two-pass linear scan
// of the data region to deduplicate (newest wins).
//
// Concurrency model — lock-free reads:
//
//	Get holds NO lock. Safety is guaranteed by the publication pattern:
//	  1. Entry bytes are written to disk (pwrite) before the bucket pointer
//	     is published via atomic.Store.
//	  2. The data region is append-only; a published entry offset is never
//	     overwritten or invalidated.
//	  3. hi.buckets is a []atomic.Uint64 — each load/store is sequentially
//	     consistent, providing the necessary memory ordering on all Go targets.
//	  4. pread coherence: on a single OS process the kernel page cache is
//	     shared, so a pwrite followed by pread from another goroutine always
//	     observes the written data.
//
//	Set/Delete serialize via hi.mu (sync.Mutex, writers-only).
type hashIndex struct {
	mu          sync.Mutex    // serializes writers; readers are lock-free
	f           *os.File
	path        string
	bucketCount uint64
	bucketBase  int64         // file offset of the first bucket slot
	dataBase    int64         // file offset of the first entry
	writePos    atomic.Int64  // next append offset
	entryCount  atomic.Int64  // live entries (informational)
	buckets     []atomic.Uint64 // in-memory bucket table; index == slot number
}

// openHashIndex opens or creates the hash index at path.
func openHashIndex(path string, bucketCount uint64) (*hashIndex, error) {
	if bucketCount == 0 {
		bucketCount = DefaultBucketCount
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open hash index %s: %w", path, err)
	}

	hi := &hashIndex{
		f:           f,
		path:        path,
		bucketCount: bucketCount,
		bucketBase:  hiHeaderSize,
		dataBase:    hiHeaderSize + int64(bucketCount)*8,
	}
	hi.writePos.Store(hi.dataBase)

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	if info.Size() == 0 {
		if err := hi.initFile(); err != nil {
			f.Close()
			return nil, fmt.Errorf("init hash index: %w", err)
		}
	} else {
		if err := hi.readHeader(); err != nil {
			f.Close()
			return nil, fmt.Errorf("read hash index header: %w", err)
		}
		hi.writePos.Store(info.Size())
	}

	if err := hi.loadBuckets(); err != nil {
		f.Close()
		return nil, fmt.Errorf("load hash index buckets: %w", err)
	}
	return hi, nil
}

// loadBuckets reads the on-disk bucket table into the in-memory atomic array.
// This is done once at open time (and after Compact). All subsequent reads
// go through hi.buckets without any syscall.
func (hi *hashIndex) loadBuckets() error {
	hi.buckets = make([]atomic.Uint64, hi.bucketCount)
	// Read the bucket table in one shot for speed.
	tableSize := int(hi.bucketCount) * 8
	buf := make([]byte, tableSize)
	if _, err := hi.f.ReadAt(buf, hi.bucketBase); err != nil {
		return err
	}
	for i := uint64(0); i < hi.bucketCount; i++ {
		v := binary.LittleEndian.Uint64(buf[i*8 : i*8+8])
		hi.buckets[i].Store(v)
	}
	return nil
}

func (hi *hashIndex) initFile() error {
	hdr := hi.encodeHeader()
	if _, err := hi.f.WriteAt(hdr, 0); err != nil {
		return err
	}
	// Zero-fill bucket table
	zeros := make([]byte, hi.bucketCount*8)
	if _, err := hi.f.WriteAt(zeros, hiHeaderSize); err != nil {
		return err
	}
	return nil
}

func (hi *hashIndex) encodeHeader() []byte {
	hdr := make([]byte, hiHeaderSize)
	copy(hdr[0:8], hiMagic)
	binary.LittleEndian.PutUint32(hdr[8:12], hiVersion)
	binary.LittleEndian.PutUint64(hdr[16:24], hi.bucketCount)
	binary.LittleEndian.PutUint64(hdr[24:32], uint64(hi.dataBase))
	binary.LittleEndian.PutUint64(hdr[32:40], uint64(hi.entryCount.Load()))
	return hdr
}

func (hi *hashIndex) readHeader() error {
	hdr := make([]byte, hiHeaderSize)
	if _, err := hi.f.ReadAt(hdr, 0); err != nil {
		return err
	}
	if string(hdr[0:8]) != hiMagic {
		return fmt.Errorf("hash index %s: bad magic %q", hi.path, hdr[0:8])
	}
	hi.bucketCount = binary.LittleEndian.Uint64(hdr[16:24])
	hi.dataBase = int64(binary.LittleEndian.Uint64(hdr[24:32]))
	hi.bucketBase = hiHeaderSize
	hi.entryCount.Store(int64(binary.LittleEndian.Uint64(hdr[32:40])))
	return nil
}

// ------------------------------------------------------------------ //
// Bucket helpers
// ------------------------------------------------------------------ //

// hashSlot returns the bucket array index for key.
func (hi *hashIndex) hashSlot(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64() % hi.bucketCount
}

func (hi *hashIndex) bucketFileOffset(slot uint64) int64 {
	return hi.bucketBase + int64(slot)*8
}

// writeBucketFile persists a bucket pointer to the file (crash recovery).
// Callers must hold hi.mu.
func (hi *hashIndex) writeBucketFile(slot uint64, entryOff int64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(entryOff))
	_, err := hi.f.WriteAt(buf[:], hi.bucketFileOffset(slot))
	return err
}

// ------------------------------------------------------------------ //
// Entry encoding / decoding
// ------------------------------------------------------------------ //

// encodeIndexEntry serialises an index entry into buf, growing it if needed.
// The caller owns buf and must not use it after returning it to a pool.
func encodeIndexEntry(buf *[]byte, next int64, key string, addr NeedleAddr, expiresAt int64, deleted bool) {
	need := 8 + 2 + len(key) + 4 + 8 + 8 + 8 + 1
	if cap(*buf) < need {
		*buf = make([]byte, need)
	} else {
		*buf = (*buf)[:need]
	}
	b := *buf
	o := 0
	binary.LittleEndian.PutUint64(b[o:], uint64(next)); o += 8
	binary.LittleEndian.PutUint16(b[o:], uint16(len(key))); o += 2
	copy(b[o:], key); o += len(key)
	binary.LittleEndian.PutUint32(b[o:], addr.VolumeID); o += 4
	binary.LittleEndian.PutUint64(b[o:], addr.Offset); o += 8
	binary.LittleEndian.PutUint64(b[o:], addr.Size); o += 8
	binary.LittleEndian.PutUint64(b[o:], uint64(expiresAt)); o += 8
	if deleted {
		b[o] = 1
	} else {
		b[o] = 0
	}
}

type idxEntry struct {
	next      int64
	key       string
	addr      NeedleAddr
	expiresAt int64
	deleted   bool
	size      int64 // total encoded size (for scanning)
}

// entryReadAheadSize is the number of bytes read in a single pread for
// readEntryAt.  It covers entries whose key is up to 214 bytes long without
// needing a second read (10 header + 214 key + 29 fixed tail = 253 ≤ 256).
const entryReadAheadSize = 256

func (hi *hashIndex) readEntryAt(pos int64) (idxEntry, error) {
	// Single pread — read up to entryReadAheadSize bytes.  For keys ≤ 214 bytes
	// this avoids a second syscall entirely (fast path).  Larger keys fall back
	// to a targeted second read (slow path).
	bp := indexEntryBufPool.Get().(*[]byte)
	if cap(*bp) < entryReadAheadSize {
		*bp = make([]byte, entryReadAheadSize)
	} else {
		*bp = (*bp)[:entryReadAheadSize]
	}
	n, err := hi.f.ReadAt(*bp, pos)
	if n < 10 {
		indexEntryBufPool.Put(bp)
		if err != nil {
			return idxEntry{}, fmt.Errorf("hash index read at %d: %w", pos, err)
		}
		return idxEntry{}, fmt.Errorf("hash index short read at %d: got %d bytes", pos, n)
	}

	b := (*bp)[:n]
	next := int64(binary.LittleEndian.Uint64(b[0:8]))
	keyLen := int(binary.LittleEndian.Uint16(b[8:10]))
	totalSize := int64(8 + 2 + keyLen + 4 + 8 + 8 + 8 + 1)

	var full []byte
	if int64(n) >= totalSize {
		// Fast path: entire entry is already in the buffer.
		full = b[:totalSize]
	} else {
		// Slow path: entry didn't fit (key > 214 bytes).  Read the exact size.
		fb := make([]byte, totalSize)
		copy(fb, b[:n])
		indexEntryBufPool.Put(bp)
		bp = nil
		if _, err := hi.f.ReadAt(fb[n:], pos+int64(n)); err != nil {
			return idxEntry{}, fmt.Errorf("hash index read tail at %d: %w", pos, err)
		}
		full = fb
	}

	o := 10
	key := string(full[o : o+keyLen]); o += keyLen // string() copies bytes
	volID := binary.LittleEndian.Uint32(full[o:]); o += 4
	offset := binary.LittleEndian.Uint64(full[o:]); o += 8
	sz := binary.LittleEndian.Uint64(full[o:]); o += 8
	expiresAt := int64(binary.LittleEndian.Uint64(full[o:])); o += 8
	deleted := full[o] != 0

	if bp != nil {
		indexEntryBufPool.Put(bp)
	}

	return idxEntry{
		next:      next,
		key:       key,
		addr:      NeedleAddr{VolumeID: volID, Offset: offset, Size: sz},
		expiresAt: expiresAt,
		deleted:   deleted,
		size:      totalSize,
	}, nil
}

// ------------------------------------------------------------------ //
// Public API
// ------------------------------------------------------------------ //

// Get returns the needle address and expiry for key.
// Returns (NeedleAddr{}, 0, false) if the key is not present or is deleted.
//
// Get is lock-free: it reads the bucket pointer via atomic.Load and follows
// the entry chain via pread. Safety is guaranteed by the publication pattern
// in Set/Delete — see the type-level comment for details.
func (hi *hashIndex) Get(key string) (NeedleAddr, int64, bool) {
	slot := hi.hashSlot(key)
	head := int64(hi.buckets[slot].Load())
	if head == 0 {
		return NeedleAddr{}, 0, false
	}

	for off := head; off != 0; {
		e, err := hi.readEntryAt(off)
		if err != nil {
			return NeedleAddr{}, 0, false
		}
		if e.key == key {
			if e.deleted {
				return NeedleAddr{}, 0, false
			}
			return e.addr, e.expiresAt, true
		}
		off = e.next
	}
	return NeedleAddr{}, 0, false
}

// Set writes or updates key with the given needle address and expiry.
func (hi *hashIndex) Set(key string, addr NeedleAddr, expiresAt int64) error {
	hi.mu.Lock()
	defer hi.mu.Unlock()

	slot := hi.hashSlot(key)
	oldHead := int64(hi.buckets[slot].Load())

	// Write the full entry to disk BEFORE publishing the bucket pointer.
	// This guarantees that any reader that observes the new pointer can
	// safely pread the entry (publication pattern).
	bp := indexEntryBufPool.Get().(*[]byte)
	encodeIndexEntry(bp, oldHead, key, addr, expiresAt, false)
	entry := *bp
	newOff := hi.writePos.Load()
	_, err := hi.f.WriteAt(entry, newOff)
	indexEntryBufPool.Put(bp)
	if err != nil {
		return fmt.Errorf("hash index set %q: %w", key, err)
	}
	hi.writePos.Add(int64(len(entry)))
	hi.entryCount.Add(1)

	// Atomically publish the new head — visible to lock-free Get callers.
	hi.buckets[slot].Store(uint64(newOff))

	// Persist bucket pointer for crash recovery (the in-memory array is
	// rebuilt from this file on the next open).
	return hi.writeBucketFile(slot, newOff)
}

// Delete appends a tombstone entry for key.
func (hi *hashIndex) Delete(key string) error {
	hi.mu.Lock()
	defer hi.mu.Unlock()

	slot := hi.hashSlot(key)
	oldHead := int64(hi.buckets[slot].Load())

	bp := indexEntryBufPool.Get().(*[]byte)
	encodeIndexEntry(bp, oldHead, key, NeedleAddr{}, 0, true)
	entry := *bp
	newOff := hi.writePos.Load()
	_, err := hi.f.WriteAt(entry, newOff)
	indexEntryBufPool.Put(bp)
	if err != nil {
		return fmt.Errorf("hash index delete %q: %w", key, err)
	}
	hi.writePos.Add(int64(len(entry)))
	if hi.entryCount.Load() > 0 {
		hi.entryCount.Add(-1)
	}

	hi.buckets[slot].Store(uint64(newOff))
	return hi.writeBucketFile(slot, newOff)
}

// forEachThreshold is the entry count above which ForEach switches from the
// fast sequential-scan strategy to the memory-safe bucket-chain strategy.
// Below this threshold the data region is small enough (~4 MiB for 100K
// entries) that loading it into a map is faster than traversing 8 MiB of
// bucket-table slots.  Above it the map allocation becomes the bottleneck and
// risks OOM on constrained hosts.
const forEachThreshold = 100_000

// ForEach calls fn for every live, non-expired entry in the index.
//
// Strategy is chosen automatically based on entryCount:
//
//   - Small index (< forEachThreshold): sequential data-region scan into a
//     map[string]latestEntry — fastest, O(N) RAM.
//   - Large index (≥ forEachThreshold): bucket-chain traversal — O(max chain
//     depth) RAM (typically a handful of strings), ~3× slower due to the fixed
//     8 MiB bucket-table scan cost, but safe for millions of keys.
//
// ForEach takes a snapshot of writePos at entry so concurrent writes that
// arrive during iteration are not observed (consistent read snapshot).
// fn must not call Set or Delete (would deadlock on hi.mu).
func (hi *hashIndex) ForEach(now time.Time, fn func(key string, addr NeedleAddr, expiresAt int64) bool) error {
	snapCount := hi.entryCount.Load()
	snapWritePos := hi.writePos.Load()

	if snapCount < forEachThreshold {
		return hi.forEachSmall(now, fn, snapWritePos)
	}
	return hi.forEachLarge(now, fn)
}

// forEachSmall is the fast path for small indexes: one linear pass over the
// data region builds a map of the newest entry per key, then a second pass
// yields live non-expired entries.  RAM: O(entryCount).
func (hi *hashIndex) forEachSmall(now time.Time, fn func(key string, addr NeedleAddr, expiresAt int64) bool, writePos int64) error {
	type latestEntry struct {
		addr      NeedleAddr
		expiresAt int64
		deleted   bool
	}

	latest := make(map[string]latestEntry, hi.entryCount.Load())
	pos := hi.dataBase
	for pos < writePos {
		e, err := hi.readEntryAt(pos)
		if err != nil {
			return fmt.Errorf("hash index ForEach at %d: %w", pos, err)
		}
		latest[e.key] = latestEntry{e.addr, e.expiresAt, e.deleted}
		pos += e.size
	}

	for key, e := range latest {
		if e.deleted {
			continue
		}
		if e.expiresAt > 0 && now.After(time.Unix(0, e.expiresAt)) {
			continue
		}
		if !fn(key, e.addr, e.expiresAt) {
			return nil
		}
	}
	return nil
}

// forEachLarge is the memory-safe path for large indexes: it iterates every
// bucket slot via the in-memory atomic array (zero syscall) and walks each
// chain from HEAD (newest) to tail.  Because entries are prepended on write,
// the first occurrence of a key in a chain is always its newest version.
// RAM: O(max chain depth) — a small []string reused per slot.
func (hi *hashIndex) forEachLarge(now time.Time, fn func(key string, addr NeedleAddr, expiresAt int64) bool) error {
	nowNs := now.UnixNano()
	seen := make([]string, 0, 8) // reused per slot

	for slot := uint64(0); slot < hi.bucketCount; slot++ {
		head := int64(hi.buckets[slot].Load())
		if head == 0 {
			continue
		}

		seen = seen[:0]
		for off := head; off != 0; {
			e, err := hi.readEntryAt(off)
			if err != nil {
				return fmt.Errorf("hash index ForEach at %d: %w", off, err)
			}

			alreadySeen := false
			for _, k := range seen {
				if k == e.key {
					alreadySeen = true
					break
				}
			}

			if !alreadySeen {
				seen = append(seen, e.key)
				if !e.deleted && !(e.expiresAt > 0 && nowNs > e.expiresAt) {
					if !fn(e.key, e.addr, e.expiresAt) {
						return nil
					}
				}
			}

			off = e.next
		}
	}
	return nil
}

// Compact rewrites the index into a new file (removing tombstones and
// stale versions) and atomically replaces the current file.
// The new bucket count is max(DefaultBucketCount, 2 × live_entries).
// Caller must hold the database write lock.
//
// No intermediate key map is allocated. Live entries are streamed directly
// from the bucket-chain iterator into the new index file.
func (hi *hashIndex) Compact(now time.Time) error {
	// Use the current entry count as an upper bound for bucket sizing.
	estEntries := hi.entryCount.Load()

	newBuckets := DefaultBucketCount
	if need := uint64(estEntries) * 2; need > newBuckets {
		// Round up to next power of two.
		newBuckets = need
		newBuckets--
		for i := 1; i < 64; i <<= 1 {
			newBuckets |= newBuckets >> i
		}
		newBuckets++
	}

	tmpPath := hi.path + ".tmp"
	newHI, err := openHashIndex(tmpPath, newBuckets)
	if err != nil {
		return fmt.Errorf("hash index compact: open tmp: %w", err)
	}

	// Stream live entries directly into the new index — no intermediate map.
	var liveCount int64
	var setErr error
	iterErr := hi.ForEach(now, func(key string, addr NeedleAddr, expiresAt int64) bool {
		if err := newHI.Set(key, addr, expiresAt); err != nil {
			setErr = err
			return false
		}
		liveCount++
		return true
	})
	if iterErr != nil || setErr != nil {
		newHI.Close()
		os.Remove(tmpPath)
		if setErr != nil {
			return fmt.Errorf("hash index compact: write: %w", setErr)
		}
		return fmt.Errorf("hash index compact: %w", iterErr)
	}

	// Flush and sync before the rename.
	if err := newHI.f.Sync(); err != nil {
		newHI.Close()
		os.Remove(tmpPath)
		return err
	}
	newHI.Close()

	// Atomically replace the current index file.
	if err := os.Rename(tmpPath, hi.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("hash index compact: rename: %w", err)
	}

	// Reopen the compacted file and reinitialize all in-memory state.
	// hi.mu is held here because db.mu.LockAll() is held by the caller,
	// ensuring no concurrent reads or writes are in flight.
	hi.mu.Lock()
	defer hi.mu.Unlock()

	hi.f.Close()
	f, err := os.OpenFile(hi.path, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("hash index compact: reopen: %w", err)
	}
	info, _ := f.Stat()
	hi.f = f
	hi.bucketCount = newBuckets
	hi.dataBase = hiHeaderSize + int64(newBuckets)*8
	hi.bucketBase = hiHeaderSize
	hi.writePos.Store(info.Size())
	hi.entryCount.Store(liveCount)

	// Rebuild the in-memory bucket array from the compacted file.
	if err := hi.loadBuckets(); err != nil {
		return fmt.Errorf("hash index compact: reload buckets: %w", err)
	}
	return nil
}

// Close closes the underlying file.
func (hi *hashIndex) Close() error {
	hi.mu.Lock()
	defer hi.mu.Unlock()
	// Persist the entry count in the header before closing
	hdr := hi.encodeHeader()
	hi.f.WriteAt(hdr, 0) //nolint:errcheck
	return hi.f.Close()
}

// loadFactor returns the ratio of live entries to bucket slots.
// Used to decide when Compact should expand the bucket table.
func (hi *hashIndex) loadFactor() float64 {
	if hi.bucketCount == 0 {
		return 0
	}
	return float64(hi.entryCount.Load()) / float64(hi.bucketCount)
}

// size returns the number of live entries.
func (hi *hashIndex) size() int64 {
	return hi.entryCount.Load()
}

// writePos returns the current append position (end of data region).
// Exposed for tests.
func (hi *hashIndex) appendPos() int64 {
	return hi.writePos.Load()
}

// ------------------------------------------------------------------ //
// Atomic bucket load helper (used by tests)
// ------------------------------------------------------------------ //

// loadBucketAtomic returns the head entry offset for the given slot.
func (hi *hashIndex) loadBucketAtomic(slot uint64) int64 {
	return int64(hi.buckets[slot].Load())
}

// atomicLoadInt64 is a thin wrapper so callers can read writePos without
// accessing the unexported field directly.  Used in ForEach snapshot.
func atomicLoadInt64(p *atomic.Int64) int64 { return p.Load() }
