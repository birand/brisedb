package brisedb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

/* Map string:string */
type Map = map[string]string

// BriseDB encapsulates the database state
type BriseDB struct {
	store        map[string]string
	valueCounts  map[string]int
	transactions *TransactionStack
	walFile      *os.File
}

// NewBriseDB creates a new BriseDB instance
func NewBriseDB() (*BriseDB, error) {
	walFile, err := os.OpenFile("wal.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open WAL file: %w", err)
	}

	db := &BriseDB{
		store:        make(map[string]string),
		valueCounts:  make(map[string]int),
		transactions: &TransactionStack{},
		walFile:      walFile,
	}

	if err := db.replayWAL(); err != nil {
		return nil, fmt.Errorf("failed to replay WAL: %w", err)
	}

	return db, nil
}

// replayWAL replays the operations from the WAL file
func (db *BriseDB) replayWAL() error {
	// Seek to the beginning of the file before replaying
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

/* Transaction points to a key:value storage */
type Transaction struct {
	store map[string]string // every transaction has its own local store
	next  *Transaction
}

/* TransactionStack maintains a list of active/suspended transactions */
type TransactionStack struct {
	top  *Transaction
	size int // more meta data can be saved like stack limit
}

/* PushTransaction create a new active transaction */
func (ts *TransactionStack) PushTransaction() {
	// Push a new Transaction, this is the current active transaction
	temp := Transaction{store: make(Map)}
	temp.next = ts.top
	ts.top = &temp
	ts.size++
}

/* PopTransaction deletes a transaction from stack */
func (ts *TransactionStack) PopTransaction() error {
	// Pop the Transaction from the stack, no longer active
	if ts.top == nil {
		// basically stack underflow
		return fmt.Errorf("ERROR: No Active Transactions")
	} else {
		ts.top = ts.top.next
		ts.size--
	}
	return nil
}

/* Peek returns the active transaction */
func (ts *TransactionStack) Peek() *Transaction {
	return ts.top
}

// BeginTransaction starts a new transaction.
func (db *BriseDB) BeginTransaction() {
	db.transactions.PushTransaction()
}

/*
Commit write(SET) changes to the store with TransactionStack scope
*/
func (db *BriseDB) CommitTransaction() error {
	ActiveTransaction := db.transactions.Peek()
	if ActiveTransaction == nil {
		return fmt.Errorf("INFO: Nothing to commit")
	}

	// Merge the transaction store with the parent transaction store or the main store
	for key, value := range ActiveTransaction.store {
		if ActiveTransaction.next != nil {
			ActiveTransaction.next.store[key] = value
		} else {
			// If the key already exists, decrement the count of the old value
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
	}

	return db.transactions.PopTransaction()
}

/* RollBackTransaction clears all keys SET within a transaction */
func (db *BriseDB) RollbackTransaction() error {
	return db.transactions.PopTransaction()
}

// PopTransaction removes the current transaction from the stack.
func (db *BriseDB) PopTransaction() error {
	return db.transactions.PopTransaction()
}

/* Get value of key from Store */
func (db *BriseDB) Get(key string) (string, bool) {
	// Search in transaction stores first
	tx := db.transactions.Peek()
	for tx != nil {
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

/* Set key to value */
func (db *BriseDB) Set(key string, value string) {
	ActiveTransaction := db.transactions.Peek()
	if ActiveTransaction != nil {
		ActiveTransaction.store[key] = value
	} else {
		// If the key already exists, decrement the count of the old value
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
			// What to do here? Log and continue?
			return
		}

		if _, err := db.walFile.Write(append(data, '\n')); err != nil {
			// What to do here? Log and continue?
			return
		}
	}
}

/* Count returns the number of keys that have been set to the specified value */
func (db *BriseDB) Count(value string) int {
	return db.valueCounts[value]
}

/* Delete value from Store */
func (db *BriseDB) Delete(key string) {
	ActiveTransaction := db.transactions.Peek()
	if ActiveTransaction != nil {
		delete(ActiveTransaction.store, key)
	} else {
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
			// What to do here? Log and continue?
			return
		}

		if _, err := db.walFile.Write(append(data, '\n')); err != nil {
			// What to do here? Log and continue?
			return
		}
	}
}
