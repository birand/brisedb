package brisedb

import (
	"encoding/json"
	"fmt"
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
	} else {
		// Merge into main store
		for key, value := range active.store {
			if oldValue, ok := s.db.store[key]; ok {
				s.db.valueCounts[oldValue]--
			}
			s.db.store[key] = value
			s.db.valueCounts[value]++

			op := struct {
				Type  string
				Key   string
				Value string
			}{"SET", key, value}
			data, err := json.Marshal(op)
			if err != nil {
				return fmt.Errorf("failed to marshal WAL entry: %w", err)
			}
			if _, err := s.db.walFile.Write(append(data, '\n')); err != nil {
				return fmt.Errorf("failed to write to WAL file: %w", err)
			}
		}

		for key := range active.deleted {
			if oldValue, ok := s.db.store[key]; ok {
				s.db.valueCounts[oldValue]--
				delete(s.db.store, key)

				op := struct {
					Type string
					Key  string
				}{"DELETE", key}
				data, err := json.Marshal(op)
				if err != nil {
					return fmt.Errorf("failed to marshal WAL entry: %w", err)
				}
				if _, err := s.db.walFile.Write(append(data, '\n')); err != nil {
					return fmt.Errorf("failed to write to WAL file: %w", err)
				}
			}
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

	if val, ok := s.db.store[key]; ok {
		return val, true
	}
	return "", false
}

// Set assigns value to key.
func (s *Session) Set(key string, value string) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

	active := s.transactions.Peek()
	if active != nil {
		delete(active.deleted, key)
		active.store[key] = value
		return nil
	}

	if oldValue, ok := s.db.store[key]; ok {
		s.db.valueCounts[oldValue]--
	}
	s.db.store[key] = value
	s.db.valueCounts[value]++

	op := struct {
		Type  string
		Key   string
		Value string
	}{"SET", key, value}
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("failed to marshal WAL entry: %w", err)
	}
	if _, err := s.db.walFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write to WAL file: %w", err)
	}
	return nil
}

// Delete removes key from the store.
func (s *Session) Delete(key string) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

	active := s.transactions.Peek()
	if active != nil {
		delete(active.store, key)
		active.deleted[key] = true
		return nil
	}

	if oldValue, ok := s.db.store[key]; ok {
		s.db.valueCounts[oldValue]--
	}
	delete(s.db.store, key)

	op := struct {
		Type string
		Key  string
	}{"DELETE", key}
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("failed to marshal WAL entry: %w", err)
	}
	if _, err := s.db.walFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write to WAL file: %w", err)
	}
	return nil
}

// Count returns the number of keys set to value, accounting for in-flight transactions.
func (s *Session) Count(value string) int {
	s.db.mu.RLock()
	defer s.db.mu.RUnlock()

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
		return s.db.valueCounts[value]
	}

	count := s.db.valueCounts[value]

	for key := range touchedKeys {
		committedVal, committedExists := s.db.store[key]
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
