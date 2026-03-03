package brisedb

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Session holds per-connection transaction state while sharing the underlying BriseDB.
type Session struct {
	db           *BriseDB
	transactions *TransactionStack
}

// BeginTransaction starts a new transaction.
func (s *Session) BeginTransaction() {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.transactions.PushTransaction()
}

// CommitTransaction writes SET/DELETE changes to the parent transaction or main store.
func (s *Session) CommitTransaction() error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

	active := s.transactions.Peek()
	if active == nil {
		return fmt.Errorf("INFO: Nothing to commit")
	}

	if active.next != nil {
		// Merge into parent transaction (no disk I/O)
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
	} else {
		// Merge into main store
		for key, value := range active.store {
			// Write value to volume
			addr, err := s.db.volumes.Write([]byte(value))
			if err != nil {
				return fmt.Errorf("commit: write volume: %w", err)
			}
			s.db.index[key] = addr

			var expiresAt int64
			if exp, ok := active.expiry[key]; ok {
				if exp.IsZero() {
					delete(s.db.expiry, key)
				} else {
					s.db.expiry[key] = exp
					expiresAt = exp.Unix()
				}
			} else {
				delete(s.db.expiry, key)
			}

			walOp := walEntry{
				Type:      "SET",
				Key:       key,
				VolumeID:  addr.VolumeID,
				Offset:    addr.Offset,
				Size:      addr.Size,
				ExpiresAt: expiresAt,
			}
			if err := s.db.writeWAL(walOp); err != nil {
				return err
			}
			s.fanoutSet(key, value, expiresAt)
		}

		for key := range active.deleted {
			if _, ok := s.db.index[key]; ok {
				delete(s.db.index, key)
				if err := s.db.writeWAL(walEntry{Type: "DELETE", Key: key}); err != nil {
					return err
				}
				s.fanoutDelete(key)
			}
			delete(s.db.expiry, key)
		}
	}

	return s.transactions.PopTransaction()
}

// RollbackTransaction discards all changes within the current transaction.
func (s *Session) RollbackTransaction() error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	return s.transactions.PopTransaction()
}

// Get returns the value of key, checking the transaction stack before the main store.
// Expired keys are treated as absent.
func (s *Session) Get(key string) (string, bool) {
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

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

	// Lazy expiry check
	if exp, ok := s.db.expiry[key]; ok && time.Now().After(exp) {
		return "", false
	}

	addr, ok := s.db.index[key]
	if !ok {
		return "", false
	}

	// Read value from volume (RLock held; fine for Phase 1)
	data, err := s.db.volumes.Read(addr)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// Set assigns value to key, clearing any existing TTL.
func (s *Session) Set(key, value string) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

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
	s.db.index[key] = addr
	delete(s.db.expiry, key)

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

	s.db.mu.Lock()
	defer s.db.mu.Unlock()

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
	s.db.index[key] = addr
	s.db.expiry[key] = exp

	if err := s.db.writeWAL(walEntry{
		Type:      "SET",
		Key:       key,
		VolumeID:  addr.VolumeID,
		Offset:    addr.Offset,
		Size:      addr.Size,
		ExpiresAt: exp.Unix(),
	}); err != nil {
		return err
	}
	s.fanoutSet(key, value, exp.Unix())
	return nil
}

// Delete removes key from the store.
func (s *Session) Delete(key string) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

	active := s.transactions.Peek()
	if active != nil {
		delete(active.store, key)
		delete(active.expiry, key)
		active.deleted[key] = true
		return nil
	}

	if _, ok := s.db.index[key]; !ok {
		return nil // key doesn't exist; nothing to do
	}
	delete(s.db.index, key)
	delete(s.db.expiry, key)

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
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

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

	if exp, ok := s.db.expiry[key]; ok {
		rem := time.Until(exp)
		if rem <= 0 {
			return -2
		}
		return int64(rem.Seconds())
	}
	if _, ok := s.db.index[key]; ok {
		return -1
	}
	return -2
}

// Persist removes the TTL from key.
// Returns true if the key existed and had a TTL.
func (s *Session) Persist(key string) bool {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

	active := s.transactions.Peek()
	if active != nil {
		if _, ok := active.store[key]; ok {
			active.expiry[key] = time.Time{}
			return true
		}
	}

	if _, ok := s.db.expiry[key]; ok {
		delete(s.db.expiry, key)
		return true
	}
	return false
}

// Count returns the number of keys whose value equals value.
// This performs a full index scan with a disk read per key — O(n).
func (s *Session) Count(value string) int {
	now := time.Now()

	// Snapshot the index and transaction state under read lock
	type entry struct {
		key       string
		addr      NeedleAddr
		exp       time.Time
		hasExp    bool
	}
	s.db.mu.RLock()
	entries := make([]entry, 0, len(s.db.index))
	for k, addr := range s.db.index {
		exp, hasExp := s.db.expiry[k]
		entries = append(entries, entry{k, addr, exp, hasExp})
	}
	// Snapshot tx overrides
	txOverride := make(map[string]string) // key → effective value ("" + deleted=true means deleted)
	txDeleted := make(map[string]bool)
	for tx := s.transactions.Peek(); tx != nil; tx = tx.next {
		for k, v := range tx.store {
			if _, seen := txOverride[k]; !seen {
				if !txDeleted[k] {
					txOverride[k] = v
				}
			}
		}
		for k := range tx.deleted {
			if _, seen := txOverride[k]; !seen {
				txDeleted[k] = true
			}
		}
	}
	s.db.mu.RUnlock()

	count := 0

	// Count from committed index (disk reads, no lock held)
	for _, e := range entries {
		if txDeleted[e.key] {
			continue
		}
		if _, overridden := txOverride[e.key]; overridden {
			continue
		}
		if e.hasExp && now.After(e.exp) {
			continue
		}
		data, err := s.db.volumes.Read(e.addr)
		if err != nil {
			continue
		}
		if string(data) == value {
			count++
		}
	}

	// Count from in-flight transaction writes
	for k, v := range txOverride {
		_ = k
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
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

	now := time.Now()
	var result []string
	for key := range s.db.index {
		if exp, ok := s.db.expiry[key]; ok && now.After(exp) {
			continue
		}
		matched, err := globMatch(pattern, key)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}
		if matched {
			result = append(result, key)
		}
	}
	sort.Strings(result)
	return result, nil
}

// Scan returns a paginated batch of committed keys starting at cursor.
// When the returned nextCursor is 0, iteration is complete.
func (s *Session) Scan(cursor, count int) (nextCursor int, keys []string) {
	if count <= 0 {
		count = 10
	}
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

	now := time.Now()
	all := make([]string, 0, len(s.db.index))
	for key := range s.db.index {
		if exp, ok := s.db.expiry[key]; ok && now.After(exp) {
			continue
		}
		all = append(all, key)
	}
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
