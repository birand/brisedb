package brisedb

import (
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
		// Merge into parent transaction
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
			if oldValue, ok := s.db.store[key]; ok {
				s.db.valueCounts[oldValue]--
			}
			s.db.store[key] = value
			s.db.valueCounts[value]++

			// Apply expiry change for this key
			op := walEntry{Type: "SET", Key: key, Value: value}
			if exp, ok := active.expiry[key]; ok {
				if exp.IsZero() {
					delete(s.db.expiry, key)
				} else {
					s.db.expiry[key] = exp
					op.ExpiresAt = exp.Unix()
				}
			} else {
				// No expiry change — clear any existing expiry (plain Set)
				delete(s.db.expiry, key)
			}

			if err := s.db.writeWAL(op); err != nil {
				return err
			}
		}

		for key := range active.deleted {
			if oldValue, ok := s.db.store[key]; ok {
				s.db.valueCounts[oldValue]--
				delete(s.db.store, key)
				if err := s.db.writeWAL(walEntry{Type: "DELETE", Key: key}); err != nil {
					return err
				}
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
// Keys that have expired are treated as absent.
func (s *Session) Get(key string) (string, bool) {
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

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

	// Lazy expiry check on committed store
	if exp, ok := s.db.expiry[key]; ok && time.Now().After(exp) {
		return "", false
	}
	if val, ok := s.db.store[key]; ok {
		return val, true
	}
	return "", false
}

// Set assigns value to key, clearing any existing TTL.
func (s *Session) Set(key string, value string) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

	active := s.transactions.Peek()
	if active != nil {
		delete(active.deleted, key)
		active.store[key] = value
		active.expiry[key] = time.Time{} // clear expiry on commit
		return nil
	}

	if oldValue, ok := s.db.store[key]; ok {
		s.db.valueCounts[oldValue]--
	}
	s.db.store[key] = value
	s.db.valueCounts[value]++
	delete(s.db.expiry, key)

	return s.db.writeWAL(walEntry{Type: "SET", Key: key, Value: value})
}

// SetEX assigns value to key with a TTL. The key is deleted after ttl elapses.
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

	if oldValue, ok := s.db.store[key]; ok {
		s.db.valueCounts[oldValue]--
	}
	s.db.store[key] = value
	s.db.valueCounts[value]++
	s.db.expiry[key] = exp

	return s.db.writeWAL(walEntry{Type: "SET", Key: key, Value: value, ExpiresAt: exp.Unix()})
}

// TTL returns the remaining lifetime of key in seconds.
// Returns -1 if the key exists but has no expiry.
// Returns -2 if the key does not exist or has already expired.
func (s *Session) TTL(key string) int64 {
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

	// Check transaction stack first
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
			// Key set in tx without TTL — check committed expiry
			// (it will be cleared on commit, so report no expiry)
			return -1
		}
		tx = tx.next
	}

	// Committed store
	if exp, ok := s.db.expiry[key]; ok {
		rem := time.Until(exp)
		if rem <= 0 {
			return -2 // expired but not yet evicted
		}
		return int64(rem.Seconds())
	}
	if _, ok := s.db.store[key]; ok {
		return -1
	}
	return -2
}

// Persist removes the TTL from key, making it persist indefinitely.
// Returns true if the key existed and had a TTL, false otherwise.
func (s *Session) Persist(key string) bool {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

	active := s.transactions.Peek()
	if active != nil {
		if _, ok := active.store[key]; ok {
			active.expiry[key] = time.Time{} // zero = clear expiry on commit
			return true
		}
	}

	if _, ok := s.db.expiry[key]; ok {
		delete(s.db.expiry, key)
		return true
	}
	return false
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

	if oldValue, ok := s.db.store[key]; ok {
		s.db.valueCounts[oldValue]--
	}
	delete(s.db.store, key)
	delete(s.db.expiry, key)

	return s.db.writeWAL(walEntry{Type: "DELETE", Key: key})
}

// Count returns the number of keys set to value, accounting for in-flight
// transactions and skipping expired keys.
func (s *Session) Count(value string) int {
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

	now := time.Now()

	// Collect all keys touched by any transaction
	touchedKeys := make(map[string]bool)
	for tx := s.transactions.Peek(); tx != nil; tx = tx.next {
		for k := range tx.store {
			touchedKeys[k] = true
		}
		for k := range tx.deleted {
			touchedKeys[k] = true
		}
	}

	if len(touchedKeys) == 0 {
		// No active transaction — use valueCounts but subtract expired keys
		count := s.db.valueCounts[value]
		for key, exp := range s.db.expiry {
			if now.After(exp) {
				if s.db.store[key] == value {
					count--
				}
			}
		}
		return count
	}

	count := s.db.valueCounts[value]

	// Subtract expired keys from the base count
	for key, exp := range s.db.expiry {
		if now.After(exp) && !touchedKeys[key] {
			if s.db.store[key] == value {
				count--
			}
		}
	}

	for key := range touchedKeys {
		committedVal, committedExists := s.db.store[key]
		// A committed key that has expired counts as absent
		if committedExists {
			if exp, ok := s.db.expiry[key]; ok && now.After(exp) {
				committedExists = false
			}
		}

		txVal, txExists := s.effectiveTransactionValue(key)

		if committedExists && committedVal == value {
			count--
		}
		if txExists && txVal == value {
			count++
		}
	}

	return count
}

// effectiveTransactionValue returns the effective value for key from the transaction stack.
// Must be called with at least db.mu.RLock held.
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

// Keys returns all committed keys whose names match pattern.
// Pattern supports glob wildcards: * (any sequence), ? (any single char).
// Expired keys are excluded. Results are sorted alphabetically.
func (s *Session) Keys(pattern string) ([]string, error) {
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

	now := time.Now()
	var result []string
	for key := range s.db.store {
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
// count is a hint for the batch size; the actual count may be smaller.
// When the returned nextCursor is 0, iteration is complete.
// Keys are iterated in alphabetical order. Expired keys are excluded.
func (s *Session) Scan(cursor, count int) (nextCursor int, keys []string) {
	if count <= 0 {
		count = 10
	}
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

	now := time.Now()
	all := make([]string, 0, len(s.db.store))
	for key := range s.db.store {
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

// globMatch reports whether key matches the glob pattern.
// * matches any sequence of characters (including /), ? matches any single character.
func globMatch(pattern, key string) (bool, error) {
	return matchGlob(pattern, key), nil
}

func matchGlob(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			// skip consecutive stars
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
