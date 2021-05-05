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

func main() {
}
