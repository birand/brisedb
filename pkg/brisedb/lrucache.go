package brisedb

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// lruCache is a thread-safe, byte-bounded LRU cache for blob data.
//
// Entries are keyed by NeedleAddr (the physical blob address), which is
// unique and immutable — brisedb never overwrites a blob at an existing
// address, it always appends a new one. This means cached entries are
// always valid; no cache invalidation is needed on key updates or deletes.
//
// The cache is bounded by total byte size rather than entry count, since
// blobs can range from a few bytes to several gigabytes.
type lruCache struct {
	mu       sync.Mutex
	maxBytes uint64
	curBytes uint64
	list     *list.List               // front = MRU, back = LRU
	items    map[NeedleAddr]*list.Element

	// stats (updated atomically so they can be read without mu)
	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64
}

type lruEntry struct {
	key   NeedleAddr
	value []byte
}

// newLRUCache creates a cache bounded to maxBytes.
// Returns nil if maxBytes is 0 (disables caching).
func newLRUCache(maxBytes uint64) *lruCache {
	if maxBytes == 0 {
		return nil
	}
	return &lruCache{
		maxBytes: maxBytes,
		list:     list.New(),
		items:    make(map[NeedleAddr]*list.Element),
	}
}

// get returns the cached blob for addr, moving it to the front (MRU).
// Returns (nil, false) on a cache miss.
func (c *lruCache) get(addr NeedleAddr) ([]byte, bool) {
	c.mu.Lock()
	el, ok := c.items[addr]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}
	c.list.MoveToFront(el)
	data := el.Value.(*lruEntry).value
	c.mu.Unlock()
	c.hits.Add(1)
	return data, true
}

// set inserts or refreshes addr → data. If inserting would exceed maxBytes,
// the least-recently-used entries are evicted first.
// Blobs larger than maxBytes are silently skipped (un-cacheable).
func (c *lruCache) set(addr NeedleAddr, data []byte) {
	size := uint64(len(data))
	if size > c.maxBytes {
		return // single blob too large to ever fit
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Refresh existing entry.
	if el, ok := c.items[addr]; ok {
		c.list.MoveToFront(el)
		old := el.Value.(*lruEntry)
		c.curBytes -= uint64(len(old.value))
		old.value = data
		c.curBytes += size
		return
	}

	// Evict LRU entries until there is room.
	for c.curBytes+size > c.maxBytes {
		c.evictLRU()
	}

	entry := &lruEntry{key: addr, value: data}
	el := c.list.PushFront(entry)
	c.items[addr] = el
	c.curBytes += size
}

// Stats returns a snapshot of cache metrics.
func (c *lruCache) Stats() LRUStats {
	c.mu.Lock()
	cur, max, entries := c.curBytes, c.maxBytes, len(c.items)
	c.mu.Unlock()
	return LRUStats{
		Entries:   entries,
		BytesUsed: cur,
		BytesMax:  max,
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
	}
}

// LRUStats is a snapshot of cache performance metrics.
type LRUStats struct {
	Entries   int
	BytesUsed uint64
	BytesMax  uint64
	Hits      int64
	Misses    int64
	Evictions int64
}

// HitRate returns hits / (hits + misses), or 0 if no lookups yet.
func (s LRUStats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// evictLRU removes the least-recently-used entry. Caller must hold mu.
func (c *lruCache) evictLRU() {
	el := c.list.Back()
	if el == nil {
		return
	}
	entry := el.Value.(*lruEntry)
	c.list.Remove(el)
	delete(c.items, entry.key)
	c.curBytes -= uint64(len(entry.value))
	c.evictions.Add(1)
}
