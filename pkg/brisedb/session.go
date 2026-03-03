package brisedb

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// Session holds per-connection transaction state while sharing the underlying BriseDB.
type Session struct {
	db           *BriseDB
	transactions *TransactionStack
}

// BeginTransaction starts a new transaction.
// No lock needed — the transaction stack is per-session.
func (s *Session) BeginTransaction() {
	s.transactions.PushTransaction()
}

// CommitTransaction writes SET/DELETE changes to the parent transaction or main store.
func (s *Session) CommitTransaction() error {
	active := s.transactions.Peek()
	if active == nil {
		return fmt.Errorf("INFO: Nothing to commit")
	}

	if active.next != nil {
		// Merge into parent transaction — no shared state touched, no lock needed.
		for key, value := range active.store {
			delete(active.next.deleted, key)
			active.next.store[key] = value
		}
		for key := range active.deleted {
			delete(active.next.store, key)
			active.next.deleted[key] = true
		}
		for key, exp := range active.expiry {
			active.next.expiry[key] = exp
		}
		return s.transactions.PopTransaction()
	}

	// Merge into main store.
	// Collect all affected keys so we can lock only the relevant shards.
	allKeys := make([]string, 0, len(active.store)+len(active.deleted))
	for k := range active.store {
		allKeys = append(allKeys, k)
	}
	for k := range active.deleted {
		allKeys = append(allKeys, k)
	}

	// Write blobs outside the lock — volumes has its own synchronisation.
	type pendingSet struct {
		key       string
		value     string
		addr      NeedleAddr
		expiresAt int64
	}
	sets := make([]pendingSet, 0, len(active.store))
	for key, value := range active.store {
		addr, err := s.db.volumes.Write([]byte(value))
		if err != nil {
			return fmt.Errorf("commit: write volume: %w", err)
		}
		var expiresAt int64
		if exp, ok := active.expiry[key]; ok && !exp.IsZero() {
			expiresAt = exp.UnixNano()
		}
		sets = append(sets, pendingSet{key, value, addr, expiresAt})
	}

	// Lock only the shards we need, in sorted order.
	idxs := s.db.mu.lockShards(allKeys)

	for _, ps := range sets {
		si := shardIndex(ps.key)
		if ps.expiresAt > 0 {
			s.db.expiry[si][ps.key] = time.Unix(0, ps.expiresAt)
		} else {
			delete(s.db.expiry[si], ps.key)
		}
		if err := s.db.hindex.Set(ps.key, ps.addr, ps.expiresAt); err != nil {
			s.db.mu.unlockShards(idxs)
			return fmt.Errorf("commit: update index: %w", err)
		}
	}

	for key := range active.deleted {
		si := shardIndex(key)
		if _, _, ok := s.db.hindex.Get(key); ok {
			if err := s.db.hindex.Delete(key); err != nil {
				s.db.mu.unlockShards(idxs)
				return fmt.Errorf("commit: delete index: %w", err)
			}
		}
		delete(s.db.expiry[si], key)
	}

	s.db.mu.unlockShards(idxs)

	// WAL writes are serialised by walMu inside writeWAL.
	for _, ps := range sets {
		if err := s.db.writeWAL(walEntry{
			Type:      "SET",
			Key:       ps.key,
			VolumeID:  ps.addr.VolumeID,
			Offset:    ps.addr.Offset,
			Size:      ps.addr.Size,
			ExpiresAt: ps.expiresAt,
		}); err != nil {
			return err
		}
		s.fanoutSet(ps.key, ps.value, ps.expiresAt)
	}
	for key := range active.deleted {
		if err := s.db.writeWAL(walEntry{Type: "DELETE", Key: key}); err != nil {
			return err
		}
		s.fanoutDelete(key)
	}

	return s.transactions.PopTransaction()
}

// RollbackTransaction discards all changes within the current transaction.
// No lock needed — the transaction stack is per-session.
func (s *Session) RollbackTransaction() error {
	return s.transactions.PopTransaction()
}

// Get returns the value of key, checking the transaction stack before the main store.
// Expired keys are treated as absent.
func (s *Session) Get(key string) (string, bool) {
	s.db.mu.RLock(key)
	defer s.db.mu.RUnlock(key)

	// Check transaction stack first (in-memory, no disk I/O)
	tx := s.transactions.Peek()
	for tx != nil {
		if tx.deleted[key] {
			return "", false
		}
		if val, ok := tx.store[key]; ok {
			return val, true
		}
		tx = tx.next
	}

	addr, expiresAt, ok := s.db.hindex.Get(key)
	if !ok {
		return "", false
	}
	if expiresAt > 0 && time.Now().After(time.Unix(0, expiresAt)) {
		return "", false
	}

	data, err := s.db.volumes.Read(addr)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// Set assigns value to key, clearing any existing TTL.
func (s *Session) Set(key, value string) error {
	active := s.transactions.Peek()
	if active != nil {
		delete(active.deleted, key)
		active.store[key] = value
		active.expiry[key] = time.Time{} // clear expiry on commit
		return nil
	}

	addr, err := s.db.volumes.Write([]byte(value))
	if err != nil {
		return fmt.Errorf("set: write volume: %w", err)
	}

	s.db.mu.Lock(key)
	if err := s.db.hindex.Set(key, addr, 0); err != nil {
		s.db.mu.Unlock(key)
		return fmt.Errorf("set: update index: %w", err)
	}
	delete(s.db.expiry[shardIndex(key)], key)
	s.db.mu.Unlock(key)

	if err := s.db.writeWAL(walEntry{
		Type:     "SET",
		Key:      key,
		VolumeID: addr.VolumeID,
		Offset:   addr.Offset,
		Size:     addr.Size,
	}); err != nil {
		return err
	}
	s.fanoutSet(key, value, 0)
	return nil
}

// SetEX assigns value to key with a TTL.
func (s *Session) SetEX(key, value string, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("TTL must be positive")
	}
	exp := time.Now().Add(ttl)

	active := s.transactions.Peek()
	if active != nil {
		delete(active.deleted, key)
		active.store[key] = value
		active.expiry[key] = exp
		return nil
	}

	addr, err := s.db.volumes.Write([]byte(value))
	if err != nil {
		return fmt.Errorf("setex: write volume: %w", err)
	}

	s.db.mu.Lock(key)
	if err := s.db.hindex.Set(key, addr, exp.UnixNano()); err != nil {
		s.db.mu.Unlock(key)
		return fmt.Errorf("setex: update index: %w", err)
	}
	s.db.expiry[shardIndex(key)][key] = exp
	s.db.mu.Unlock(key)

	if err := s.db.writeWAL(walEntry{
		Type:      "SET",
		Key:       key,
		VolumeID:  addr.VolumeID,
		Offset:    addr.Offset,
		Size:      addr.Size,
		ExpiresAt: exp.UnixNano(),
	}); err != nil {
		return err
	}
	s.fanoutSet(key, value, exp.UnixNano())
	return nil
}

// ------------------------------------------------------------------ //
// Blob API — direct byte-slice I/O, bypasses transaction buffer.
// Use these for large values (>1 MB) where buffering in tx.store
// would cause excessive memory pressure.
// ------------------------------------------------------------------ //

// BlobMeta holds metadata returned by GetBlobAddr.
type BlobMeta struct {
	Addr      NeedleAddr
	ExpiresAt int64 // Unix nanoseconds; 0 = no expiry
}

// SetBlob writes data directly to the volume and updates the index.
// Any active transaction is ignored — this is always an immediate commit.
// ttl <= 0 means no expiry.
func (s *Session) SetBlob(key string, data []byte, ttl time.Duration) error {
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}

	addr, err := s.db.volumes.Write(data)
	if err != nil {
		return fmt.Errorf("setblob: write volume: %w", err)
	}

	var expiresAt int64
	si := shardIndex(key)

	s.db.mu.Lock(key)
	if !exp.IsZero() {
		expiresAt = exp.UnixNano()
		s.db.expiry[si][key] = exp
	} else {
		delete(s.db.expiry[si], key)
	}
	if err := s.db.hindex.Set(key, addr, expiresAt); err != nil {
		s.db.mu.Unlock(key)
		return fmt.Errorf("setblob: update index: %w", err)
	}
	s.db.mu.Unlock(key)

	if err := s.db.writeWAL(walEntry{
		Type:      "SET",
		Key:       key,
		VolumeID:  addr.VolumeID,
		Offset:    addr.Offset,
		Size:      addr.Size,
		ExpiresAt: expiresAt,
	}); err != nil {
		return err
	}
	s.fanoutSet(key, string(data), expiresAt)
	return nil
}

// SetBlobStream streams size bytes from r directly into the volume, writing
// the 8-byte length header first so no RAM buffer is needed.
// The caller must supply the exact size (i.e. Content-Length must be known).
// Any active transaction is ignored — this is always an immediate commit.
// ttl <= 0 means no expiry.
func (s *Session) SetBlobStream(key string, r io.Reader, size int64, ttl time.Duration) error {
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}

	addr, err := s.db.volumes.WriteStream(r, size)
	if err != nil {
		return fmt.Errorf("setblobstream: write volume: %w", err)
	}

	var expiresAt int64
	si := shardIndex(key)

	s.db.mu.Lock(key)
	if !exp.IsZero() {
		expiresAt = exp.UnixNano()
		s.db.expiry[si][key] = exp
	} else {
		delete(s.db.expiry[si], key)
	}
	if err := s.db.hindex.Set(key, addr, expiresAt); err != nil {
		s.db.mu.Unlock(key)
		return fmt.Errorf("setblobstream: update index: %w", err)
	}
	s.db.mu.Unlock(key)

	if err := s.db.writeWAL(walEntry{
		Type:      "SET",
		Key:       key,
		VolumeID:  addr.VolumeID,
		Offset:    addr.Offset,
		Size:      addr.Size,
		ExpiresAt: expiresAt,
	}); err != nil {
		return err
	}
	return nil
}

// GetBlobMeta returns the volume address and expiry for key without reading
// the blob data.  Returns (BlobMeta{}, false) if the key is absent or expired.
func (s *Session) GetBlobMeta(key string) (BlobMeta, bool) {
	s.db.mu.RLock(key)
	defer s.db.mu.RUnlock(key)

	addr, expiresAt, ok := s.db.hindex.Get(key)
	if !ok {
		return BlobMeta{}, false
	}
	if expiresAt > 0 && time.Now().After(time.Unix(0, expiresAt)) {
		return BlobMeta{}, false
	}
	return BlobMeta{Addr: addr, ExpiresAt: expiresAt}, true
}

// ReadBlobAt reads Size bytes from addr, returning a sub-slice [start, start+length).
// Used by the HTTP blob server to serve Range requests without loading the full blob.
// start and length are clamped to the actual data bounds.
func (s *Session) ReadBlobAt(addr NeedleAddr, start, length int64) ([]byte, error) {
	data, err := s.db.volumes.Read(addr)
	if err != nil {
		return nil, err
	}
	n := int64(len(data))
	if start >= n {
		return []byte{}, nil
	}
	end := start + length
	if end > n || length < 0 {
		end = n
	}
	return data[start:end], nil
}

// Delete removes key from the store.
func (s *Session) Delete(key string) error {
	active := s.transactions.Peek()
	if active != nil {
		delete(active.store, key)
		delete(active.expiry, key)
		active.deleted[key] = true
		return nil
	}

	s.db.mu.Lock(key)
	_, _, exists := s.db.hindex.Get(key)
	if exists {
		if err := s.db.hindex.Delete(key); err != nil {
			s.db.mu.Unlock(key)
			return fmt.Errorf("delete: update index: %w", err)
		}
		delete(s.db.expiry[shardIndex(key)], key)
	}
	s.db.mu.Unlock(key)

	if !exists {
		return nil
	}
	if err := s.db.writeWAL(walEntry{Type: "DELETE", Key: key}); err != nil {
		return err
	}
	s.fanoutDelete(key)
	return nil
}

// TTL returns the remaining lifetime of key in seconds.
// Returns -1 if the key exists but has no expiry.
// Returns -2 if the key does not exist or has already expired.
func (s *Session) TTL(key string) int64 {
	s.db.mu.RLock(key)
	defer s.db.mu.RUnlock(key)

	tx := s.transactions.Peek()
	for tx != nil {
		if tx.deleted[key] {
			return -2
		}
		if _, ok := tx.store[key]; ok {
			if exp, hasExp := tx.expiry[key]; hasExp && !exp.IsZero() {
				rem := time.Until(exp)
				if rem <= 0 {
					return -2
				}
				return int64(rem.Seconds())
			}
			return -1
		}
		tx = tx.next
	}

	_, expiresAt, ok := s.db.hindex.Get(key)
	if !ok {
		return -2
	}
	if expiresAt > 0 {
		rem := time.Until(time.Unix(0, expiresAt))
		if rem <= 0 {
			return -2
		}
		return int64(rem.Seconds())
	}
	return -1
}

// Persist removes the TTL from key.
// Returns true if the key existed and had a TTL.
func (s *Session) Persist(key string) bool {
	active := s.transactions.Peek()
	if active != nil {
		if _, ok := active.store[key]; ok {
			active.expiry[key] = time.Time{}
			return true
		}
	}

	s.db.mu.Lock(key)
	defer s.db.mu.Unlock(key)

	addr, expiresAt, ok := s.db.hindex.Get(key)
	if !ok || expiresAt == 0 {
		return false
	}
	s.db.hindex.Set(key, addr, 0) //nolint:errcheck
	delete(s.db.expiry[shardIndex(key)], key)
	return true
}

// Count returns the number of keys whose value equals value.
// Performs a full index scan with one disk read per key — O(n).
func (s *Session) Count(value string) int {
	now := time.Now()

	// Snapshot tx overrides — tx stack is per-session, no shared lock needed.
	txOverride := make(map[string]string)
	txDeleted := make(map[string]bool)
	for tx := s.transactions.Peek(); tx != nil; tx = tx.next {
		for k, v := range tx.store {
			if _, seen := txOverride[k]; !seen && !txDeleted[k] {
				txOverride[k] = v
			}
		}
		for k := range tx.deleted {
			if _, seen := txDeleted[k]; !seen {
				txDeleted[k] = true
			}
		}
	}

	count := 0

	// Count from committed index (ForEach holds hindex.mu internally)
	s.db.hindex.ForEach(now, func(key string, addr NeedleAddr, _ int64) bool { //nolint:errcheck
		if txDeleted[key] {
			return true
		}
		if _, overridden := txOverride[key]; overridden {
			return true
		}
		data, err := s.db.volumes.Read(addr)
		if err != nil {
			return true
		}
		if string(data) == value {
			count++
		}
		return true
	})

	// Count from in-flight transaction writes
	for _, v := range txOverride {
		if v == value {
			count++
		}
	}

	return count
}

// Keys returns all committed keys matching pattern, excluding expired keys.
// Pattern supports * (any sequence) and ? (any single char).
// Results are sorted alphabetically.
func (s *Session) Keys(pattern string) ([]string, error) {
	// Validate pattern first
	if _, err := globMatch(pattern, ""); err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}

	var result []string
	s.db.hindex.ForEach(time.Now(), func(key string, _ NeedleAddr, _ int64) bool { //nolint:errcheck
		if matchGlob(pattern, key) {
			result = append(result, key)
		}
		return true
	})
	sort.Strings(result)
	return result, nil
}

// Scan returns a paginated batch of committed keys starting at cursor.
// When the returned nextCursor is 0, iteration is complete.
func (s *Session) Scan(cursor, count int) (nextCursor int, keys []string) {
	if count <= 0 {
		count = 10
	}

	var all []string
	s.db.hindex.ForEach(time.Now(), func(key string, _ NeedleAddr, _ int64) bool { //nolint:errcheck
		all = append(all, key)
		return true
	})
	sort.Strings(all)

	if cursor >= len(all) {
		return 0, nil
	}
	end := cursor + count
	if end >= len(all) {
		return 0, all[cursor:]
	}
	return end, all[cursor:end]
}

// ------------------------------------------------------------------ //
// Replication helpers (called with mu held)
// ------------------------------------------------------------------ //

// fanoutSet sends a SET entry (with value) to connected replicas.
func (s *Session) fanoutSet(key, value string, expiresAt int64) {
	op := walEntry{Type: "SET", Key: key, Value: value, ExpiresAt: expiresAt}
	data, _ := json.Marshal(op)
	s.db.replication.fanout(data)
}

// fanoutDelete sends a DELETE entry to connected replicas.
func (s *Session) fanoutDelete(key string) {
	op := walEntry{Type: "DELETE", Key: key}
	data, _ := json.Marshal(op)
	s.db.replication.fanout(data)
}

// effectiveTransactionValue returns the effective value for key from the tx stack.
func (s *Session) effectiveTransactionValue(key string) (string, bool) {
	for tx := s.transactions.Peek(); tx != nil; tx = tx.next {
		if tx.deleted[key] {
			return "", false
		}
		if val, ok := tx.store[key]; ok {
			return val, true
		}
	}
	return "", false
}

// ------------------------------------------------------------------ //
// Glob matching
// ------------------------------------------------------------------ //

func globMatch(pattern, key string) (bool, error) {
	return matchGlob(pattern, key), nil
}

func matchGlob(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if matchGlob(p, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}
