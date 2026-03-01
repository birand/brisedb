package brisedb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

/* Map string:string */
type Map = map[string]string

// BriseDB encapsulates the database state
type BriseDB struct {
	mu           sync.RWMutex
	store        map[string]string
	valueCounts  map[string]int
	transactions *TransactionStack
	walFile      *os.File
	walPath      string
}

// NewBriseDB creates a new BriseDB instance. walPath is the path to the WAL file.
func NewBriseDB(walPath string) (*BriseDB, error) {
	walFile, err := os.OpenFile(walPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open WAL file: %w", err)
	}

	db := &BriseDB{
		store:        make(map[string]string),
		valueCounts:  make(map[string]int),
		transactions: &TransactionStack{},
		walFile:      walFile,
		walPath:      walPath,
	}

	if err := db.replayWAL(); err != nil {
		return nil, fmt.Errorf("failed to replay WAL: %w", err)
	}

	return db, nil
}

// replayWAL replays the operations from the WAL file
func (db *BriseDB) replayWAL() error {
	if _, err := db.walFile.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek to the beginning of the WAL file: %w", err)
	}

	scanner := bufio.NewScanner(db.walFile)
	for scanner.Scan() {
		var op struct {
			Type  string
			Key   string
			Value string
		}
		if err := json.Unmarshal(scanner.Bytes(), &op); err != nil {
			return fmt.Errorf("failed to unmarshal WAL entry: %w", err)
		}

		switch op.Type {
		case "SET":
			if oldValue, ok := db.store[op.Key]; ok {
				db.valueCounts[oldValue]--
			}
			db.store[op.Key] = op.Value
			db.valueCounts[op.Value]++
		case "DELETE":
			if oldValue, ok := db.store[op.Key]; ok {
				db.valueCounts[oldValue]--
			}
			delete(db.store, op.Key)
		}
	}

	return scanner.Err()
}

// Compact rewrites the WAL with only the current committed state, discarding history.
func (db *BriseDB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	f, err := os.OpenFile(db.walPath, os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to truncate WAL: %w", err)
	}

	w := bufio.NewWriter(f)
	for key, value := range db.store {
		op := struct {
			Type  string
			Key   string
			Value string
		}{"SET", key, value}
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

// Transaction points to a key:value storage
type Transaction struct {
	store   map[string]string // keys set within this transaction
	deleted map[string]bool   // keys deleted within this transaction
	next    *Transaction
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

// BeginTransaction starts a new transaction.
func (db *BriseDB) BeginTransaction() {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.transactions.PushTransaction()
}

// CommitTransaction writes SET/DELETE changes to the parent transaction or main store.
func (db *BriseDB) CommitTransaction() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	active := db.transactions.Peek()
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
			if oldValue, ok := db.store[key]; ok {
				db.valueCounts[oldValue]--
			}
			db.store[key] = value
			db.valueCounts[value]++

			op := struct {
				Type  string
				Key   string
				Value string
			}{"SET", key, value}
			data, err := json.Marshal(op)
			if err != nil {
				return fmt.Errorf("failed to marshal WAL entry: %w", err)
			}
			if _, err := db.walFile.Write(append(data, '\n')); err != nil {
				return fmt.Errorf("failed to write to WAL file: %w", err)
			}
		}

		for key := range active.deleted {
			if oldValue, ok := db.store[key]; ok {
				db.valueCounts[oldValue]--
				delete(db.store, key)

				op := struct {
					Type string
					Key  string
				}{"DELETE", key}
				data, err := json.Marshal(op)
				if err != nil {
					return fmt.Errorf("failed to marshal WAL entry: %w", err)
				}
				if _, err := db.walFile.Write(append(data, '\n')); err != nil {
					return fmt.Errorf("failed to write to WAL file: %w", err)
				}
			}
		}
	}

	return db.transactions.PopTransaction()
}

// RollbackTransaction discards all changes within the current transaction.
func (db *BriseDB) RollbackTransaction() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.transactions.PopTransaction()
}

// Get returns the value of key, checking the transaction stack before the main store.
func (db *BriseDB) Get(key string) (string, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	tx := db.transactions.Peek()
	for tx != nil {
		if tx.deleted[key] {
			return "", false
		}
		if val, ok := tx.store[key]; ok {
			return val, true
		}
		tx = tx.next
	}

	if val, ok := db.store[key]; ok {
		return val, true
	}
	return "", false
}

// Set assigns value to key.
func (db *BriseDB) Set(key string, value string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	active := db.transactions.Peek()
	if active != nil {
		delete(active.deleted, key)
		active.store[key] = value
		return nil
	}

	if oldValue, ok := db.store[key]; ok {
		db.valueCounts[oldValue]--
	}
	db.store[key] = value
	db.valueCounts[value]++

	op := struct {
		Type  string
		Key   string
		Value string
	}{"SET", key, value}
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("failed to marshal WAL entry: %w", err)
	}
	if _, err := db.walFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write to WAL file: %w", err)
	}
	return nil
}

// Count returns the number of keys set to value, accounting for in-flight transactions.
func (db *BriseDB) Count(value string) int {
	db.mu.RLock()
	defer db.mu.RUnlock()

	// Collect all keys touched by any transaction
	touchedKeys := make(map[string]bool)
	for tx := db.transactions.Peek(); tx != nil; tx = tx.next {
		for k := range tx.store {
			touchedKeys[k] = true
		}
		for k := range tx.deleted {
			touchedKeys[k] = true
		}
	}

	if len(touchedKeys) == 0 {
		return db.valueCounts[value]
	}

	count := db.valueCounts[value]

	for key := range touchedKeys {
		// What the committed store has for this key
		committedVal, committedExists := db.store[key]

		// What the effective transaction state is for this key
		txVal, txExists := db.effectiveTransactionValue(key)

		// Adjust count: remove committed contribution, add tx contribution
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
// Must be called with at least mu.RLock held.
func (db *BriseDB) effectiveTransactionValue(key string) (string, bool) {
	for tx := db.transactions.Peek(); tx != nil; tx = tx.next {
		if tx.deleted[key] {
			return "", false
		}
		if val, ok := tx.store[key]; ok {
			return val, true
		}
	}
	return "", false
}

// Delete removes key from the store.
func (db *BriseDB) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	active := db.transactions.Peek()
	if active != nil {
		delete(active.store, key)
		active.deleted[key] = true
		return nil
	}

	if oldValue, ok := db.store[key]; ok {
		db.valueCounts[oldValue]--
	}
	delete(db.store, key)

	op := struct {
		Type string
		Key  string
	}{"DELETE", key}
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("failed to marshal WAL entry: %w", err)
	}
	if _, err := db.walFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write to WAL file: %w", err)
	}
	return nil
}
