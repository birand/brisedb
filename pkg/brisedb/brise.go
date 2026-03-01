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

// BriseDB encapsulates the shared database state (store, WAL). Thread-safe.
// Transaction state lives in Session — one per connection/client.
type BriseDB struct {
	mu          sync.RWMutex
	store       map[string]string
	valueCounts map[string]int
	walFile     *os.File
	walPath     string
}

// NewBriseDB creates a new BriseDB instance. walPath is the path to the WAL file.
func NewBriseDB(walPath string) (*BriseDB, error) {
	walFile, err := os.OpenFile(walPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open WAL file: %w", err)
	}

	db := &BriseDB{
		store:       make(map[string]string),
		valueCounts: make(map[string]int),
		walFile:     walFile,
		walPath:     walPath,
	}

	if err := db.replayWAL(); err != nil {
		return nil, fmt.Errorf("failed to replay WAL: %w", err)
	}

	return db, nil
}

// NewSession creates a new Session backed by this database.
func (db *BriseDB) NewSession() *Session {
	return &Session{
		db:           db,
		transactions: &TransactionStack{},
	}
}

// Close closes the WAL file.
func (db *BriseDB) Close() error {
	return db.walFile.Close()
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
