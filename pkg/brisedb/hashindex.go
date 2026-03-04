package brisedb

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"os"
	"sync"
	"syscall"
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
//	[8]  ExpiresAt    — Unix seconds; 0 = no expiry
//	[1]  deleted      — 0 = alive, 1 = tombstone
//
// Writes prepend to the chain (update bucket → new entry → old head), so the
// newest entry is always reachable first.  ForEach does a two-pass linear scan
// of the data region to deduplicate (newest wins).
type hashIndex struct {
	mu          sync.RWMutex
	f           *os.File
	path        string
	bucketCount uint64
	bucketBase  int64  // file offset of the first bucket slot
	dataBase    int64  // file offset of the first entry
	writePos    int64  // next append offset (= current file size)
	entryCount  int64  // live entries (informational)
	mmapData    []byte // MAP_SHARED read-only mmap of [0, dataBase); nil = fallback to pread
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
	hi.writePos = hi.dataBase

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
		hi.writePos = info.Size()
	}
	hi.mapBuckets()
	return hi, nil
}

// mapBuckets memory-maps the header + bucket table region of the index file.
// Reads from the bucket table then use direct memory access instead of pread.
// Writes still go through f.WriteAt; MAP_SHARED ensures the mmap sees them.
// On any error the mmap is silently skipped and pread is used as fallback.
func (hi *hashIndex) mapBuckets() {
	size := int(hi.dataBase) // header (64 B) + bucket table (bucketCount × 8 B)
	data, err := syscall.Mmap(int(hi.f.Fd()), 0, size,
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err == nil {
		hi.mmapData = data
	}
}

// unmapBuckets releases the mmap region if one is active.
func (hi *hashIndex) unmapBuckets() {
	if hi.mmapData != nil {
		syscall.Munmap(hi.mmapData) //nolint:errcheck
		hi.mmapData = nil
	}
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
	binary.LittleEndian.PutUint64(hdr[32:40], uint64(hi.entryCount))
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
	hi.entryCount = int64(binary.LittleEndian.Uint64(hdr[32:40]))
	return nil
}

// ------------------------------------------------------------------ //
// Bucket helpers
// ------------------------------------------------------------------ //

func (hi *hashIndex) bucketFileOffset(key string) int64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	slot := h.Sum64() % hi.bucketCount
	return hi.bucketBase + int64(slot)*8
}

func (hi *hashIndex) readBucket(boff int64) (int64, error) {
	if hi.mmapData != nil {
		// Zero-syscall path: read directly from the memory-mapped bucket table.
		return int64(binary.LittleEndian.Uint64(hi.mmapData[boff : boff+8])), nil
	}
	var buf [8]byte
	if _, err := hi.f.ReadAt(buf[:], boff); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(buf[:])), nil
}

func (hi *hashIndex) writeBucket(boff, entryOff int64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(entryOff))
	_, err := hi.f.WriteAt(buf[:], boff)
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

func (hi *hashIndex) readEntryAt(pos int64) (idxEntry, error) {
	// Read fixed prefix: next(8) + key_len(2) = 10 bytes
	var prefix [10]byte
	if _, err := hi.f.ReadAt(prefix[:], pos); err != nil {
		return idxEntry{}, fmt.Errorf("hash index read prefix at %d: %w", pos, err)
	}
	next := int64(binary.LittleEndian.Uint64(prefix[0:8]))
	keyLen := int(binary.LittleEndian.Uint16(prefix[8:10]))

	need := keyLen + 4 + 8 + 8 + 8 + 1
	bp := indexEntryBufPool.Get().(*[]byte)
	if cap(*bp) < need {
		*bp = make([]byte, need)
	} else {
		*bp = (*bp)[:need]
	}
	tail := *bp
	if _, err := hi.f.ReadAt(tail, pos+10); err != nil {
		indexEntryBufPool.Put(bp)
		return idxEntry{}, fmt.Errorf("hash index read tail at %d: %w", pos, err)
	}
	o := 0
	key := string(tail[o : o+keyLen]); o += keyLen // string() copies bytes
	volID := binary.LittleEndian.Uint32(tail[o:]); o += 4
	offset := binary.LittleEndian.Uint64(tail[o:]); o += 8
	sz := binary.LittleEndian.Uint64(tail[o:]); o += 8
	expiresAt := int64(binary.LittleEndian.Uint64(tail[o:])); o += 8
	deleted := tail[o] != 0
	indexEntryBufPool.Put(bp)

	totalSize := int64(8 + 2 + keyLen + 4 + 8 + 8 + 8 + 1)
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
func (hi *hashIndex) Get(key string) (NeedleAddr, int64, bool) {
	hi.mu.RLock()
	defer hi.mu.RUnlock()

	boff := hi.bucketFileOffset(key)
	head, err := hi.readBucket(boff)
	if err != nil || head == 0 {
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

	boff := hi.bucketFileOffset(key)
	oldHead, err := hi.readBucket(boff)
	if err != nil {
		return err
	}

	bp := indexEntryBufPool.Get().(*[]byte)
	encodeIndexEntry(bp, oldHead, key, addr, expiresAt, false)
	entry := *bp
	newOff := hi.writePos
	_, err = hi.f.WriteAt(entry, newOff)
	indexEntryBufPool.Put(bp)
	if err != nil {
		return fmt.Errorf("hash index set %q: %w", key, err)
	}
	hi.writePos += int64(len(entry))
	hi.entryCount++

	return hi.writeBucket(boff, newOff)
}

// Delete appends a tombstone entry for key.
func (hi *hashIndex) Delete(key string) error {
	hi.mu.Lock()
	defer hi.mu.Unlock()

	boff := hi.bucketFileOffset(key)
	oldHead, err := hi.readBucket(boff)
	if err != nil {
		return err
	}

	bp := indexEntryBufPool.Get().(*[]byte)
	encodeIndexEntry(bp, oldHead, key, NeedleAddr{}, 0, true)
	entry := *bp
	newOff := hi.writePos
	_, err = hi.f.WriteAt(entry, newOff)
	indexEntryBufPool.Put(bp)
	if err != nil {
		return fmt.Errorf("hash index delete %q: %w", key, err)
	}
	hi.writePos += int64(len(entry))
	if hi.entryCount > 0 {
		hi.entryCount--
	}

	return hi.writeBucket(boff, newOff)
}

// ForEach calls fn for every live, non-expired entry in the index.
// It performs two passes over the data region so that the newest entry per
// key is used (entries are stored oldest-first in the file).
// fn must not call any hashIndex method (would deadlock on the read lock).
// NOTE: Phase 3 limitation — all keys are held in memory during the scan.
func (hi *hashIndex) ForEach(now time.Time, fn func(key string, addr NeedleAddr, expiresAt int64) bool) error {
	hi.mu.RLock()
	defer hi.mu.RUnlock()

	type latestEntry struct {
		addr      NeedleAddr
		expiresAt int64
		deleted   bool
	}

	// Pass 1: scan data region; newest occurrence of each key wins because
	// later entries in the file overwrite earlier ones in our map.
	latest := make(map[string]latestEntry)
	pos := hi.dataBase
	for pos < hi.writePos {
		e, err := hi.readEntryAt(pos)
		if err != nil {
			return fmt.Errorf("hash index ForEach at %d: %w", pos, err)
		}
		latest[e.key] = latestEntry{e.addr, e.expiresAt, e.deleted}
		pos += e.size
	}

	// Pass 2: yield live, non-expired entries.
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

// Compact rewrites the index into a new file (removing tombstones and
// stale versions) and atomically replaces the current file.
// The new bucket count is max(DefaultBucketCount, 2 × live_entries).
// Caller must hold the database write lock.
func (hi *hashIndex) Compact(now time.Time) error {
	// Collect live entries (under our own lock via ForEach)
	type entry struct {
		addr      NeedleAddr
		expiresAt int64
	}
	live := make(map[string]entry)
	if err := hi.ForEach(now, func(key string, addr NeedleAddr, expiresAt int64) bool {
		live[key] = entry{addr, expiresAt}
		return true
	}); err != nil {
		return err
	}

	newBuckets := DefaultBucketCount
	if need := uint64(len(live)) * 2; need > newBuckets {
		// Round up to next power of two
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

	for key, e := range live {
		if err := newHI.Set(key, e.addr, e.expiresAt); err != nil {
			newHI.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("hash index compact: write %q: %w", key, err)
		}
	}

	// Flush and sync before the rename
	if err := newHI.f.Sync(); err != nil {
		newHI.Close()
		os.Remove(tmpPath)
		return err
	}
	newHI.Close()

	// Atomically replace the current index file
	if err := os.Rename(tmpPath, hi.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("hash index compact: rename: %w", err)
	}

	// Reopen the compacted file in-place
	hi.mu.Lock()
	defer hi.mu.Unlock()
	hi.unmapBuckets()
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
	hi.writePos = info.Size()
	hi.entryCount = int64(len(live))
	hi.mapBuckets()
	return nil
}

// Close closes the underlying file.
func (hi *hashIndex) Close() error {
	hi.mu.Lock()
	defer hi.mu.Unlock()
	hi.unmapBuckets()
	// Persist the entry count in the header before closing
	hdr := hi.encodeHeader()
	hi.f.WriteAt(hdr, 0) //nolint:errcheck
	return hi.f.Close()
}
