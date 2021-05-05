package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

/* GlobalStore holds the (global) variables */
var GlobalStore = make(map[string]string)

/* Map string:string */
type Map = map[string]string

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
func (ts *TransactionStack) PopTransaction() {
	// Pop the Transaciton from the stack, no longer active
	if ts.top == nil {
		// basically stack underflow
		fmt.Printf("ERROR: No Active Transactions\n")
	} else {
		ts.top = ts.top.next
		ts.size--
	}
}

/* Peek returns the active transaction */
func (ts *TransactionStack) Peek() *Transaction {
	return ts.top
}

/* Commit write(SET) changes to the store with TransactionStack scope
Also write cahnges to disk/file,
if data needs to persist after the shell closes
*/
func (ts *TransactionStack) Commit() {
	ActiveTransaction := ts.Peek()
	if ActiveTransaction != nil {
		for key, value := range ActiveTransaction.store {
			GlobalStore[key] = value
			if ActiveTransaction.next != nil {
				// update the parent transaction
				ActiveTransaction.next.store[key] = value
			}
		}
	} else {
		fmt.Printf("INFO: Nothing ot commit\n")
	}
	// TODO: write data to file to make it persist to disk
	// Tip: serialize map data to JSON
}
func main() {
}
