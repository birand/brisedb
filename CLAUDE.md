# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build the CLI binary
go build ./cmd/cli/

# Run the CLI
go run ./cmd/cli/

# Run all tests
go test ./...

# Run tests for a specific package
go test ./pkg/brisedb/

# Run a single test
go test ./pkg/brisedb/ -run TestTransaction

# Run tests with verbose output
go test -v ./pkg/brisedb/
```

## Architecture

brisedb is an experimental in-memory key-value store with WAL-based persistence. It has two components:

- **`pkg/brisedb/`** — the core library (`BriseDB` struct)
- **`cmd/cli/`** — interactive REPL that wraps the library

### Core data structures

`BriseDB` holds:
- `store map[string]string` — the main committed key-value store
- `valueCounts map[string]int` — reverse index tracking how many keys map to each value (used by `COUNT`)
- `transactions *TransactionStack` — stack of nested in-flight transactions
- `walFile *os.File` — append-only WAL for persistence (`wal.log` in the working directory)

### Transaction model

Transactions use a linked-list stack (`TransactionStack`). Each `Transaction` has its own local `store`. On `Set`/`Delete`, if a transaction is active, changes go to `tx.store` only. On `CommitTransaction`, the top transaction merges into either its parent transaction or the main store (and WAL). On `RollbackTransaction`, the top transaction is simply popped and discarded.

Reads (`Get`) search the transaction stack from top to bottom before falling back to the main store.

### Persistence

On startup, `NewBriseDB` opens (or creates) `wal.log` and replays it via `replayWAL`. Each committed `SET` and `DELETE` is appended as a JSON line. The WAL file path is relative to the working directory at runtime — tests truncate it to start fresh.

### CLI commands

`SET key value`, `GET key`, `DELETE key`, `COUNT value`, `BEGIN`, `COMMIT`, `ROLLBACK`, `END` (alias for ROLLBACK without clearing), `STOP`.
