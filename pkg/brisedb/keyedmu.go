package brisedb

import (
	"hash/fnv"
	"sort"
	"sync"
)

const numShards = 64

// keyedMutex is a sharded RWMutex that reduces lock contention by assigning
// each key to one of numShards buckets via FNV hash.
//
// Single-key operations lock only the relevant shard.
// Global operations (Compact, evictExpired) lock all shards in index order.
type keyedMutex struct {
	shards [numShards]sync.RWMutex
}

// shardIndex returns the shard index for key.
func shardIndex(key string) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32()%numShards)
}

func (km *keyedMutex) Lock(key string)    { km.shards[shardIndex(key)].Lock() }
func (km *keyedMutex) Unlock(key string)  { km.shards[shardIndex(key)].Unlock() }
func (km *keyedMutex) RLock(key string)   { km.shards[shardIndex(key)].RLock() }
func (km *keyedMutex) RUnlock(key string) { km.shards[shardIndex(key)].RUnlock() }

// lockShards locks the minimal set of shards covering keys, in sorted index
// order to prevent deadlocks. Returns the sorted shard indices that were locked.
func (km *keyedMutex) lockShards(keys []string) []int {
	seen := make(map[int]bool, len(keys))
	for _, k := range keys {
		seen[shardIndex(k)] = true
	}
	idxs := make([]int, 0, len(seen))
	for i := range seen {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		km.shards[i].Lock()
	}
	return idxs
}

// unlockShards releases shard locks returned by lockShards, in reverse order.
func (km *keyedMutex) unlockShards(idxs []int) {
	for i := len(idxs) - 1; i >= 0; i-- {
		km.shards[idxs[i]].Unlock()
	}
}

// LockAll locks every shard in index order. Use for global operations.
func (km *keyedMutex) LockAll() {
	for i := range km.shards {
		km.shards[i].Lock()
	}
}

// UnlockAll unlocks every shard in reverse index order.
func (km *keyedMutex) UnlockAll() {
	for i := len(km.shards) - 1; i >= 0; i-- {
		km.shards[i].Unlock()
	}
}
