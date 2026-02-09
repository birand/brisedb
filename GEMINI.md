# GEMINI.md

This file provides a comprehensive overview of the `brisedb` project, its structure, and how to interact with it.

## Project Overview

`brisedb` is an experimental, in-memory key-value database written in Go. It provides a simple command-line interface (CLI) for interacting with the database.

**Key Features:**

*   **Key-Value Store:** Basic `SET`, `GET`, and `DELETE` operations.
*   **Transactions:** Supports `BEGIN`, `COMMIT`, and `ROLLBACK` for ACID-like properties.
*   **Value Counting:** A `COUNT` operation to find the number of keys with a given value.
*   **Persistence:** Uses a Write-Ahead Log (WAL) to persist data and recover state upon restart.
*   **Concurrency Safe:** All database operations are thread-safe.

**Architecture:**

*   **`pkg/brisedb/brise.go`:** Contains the core database logic, including data storage, transaction management, and WAL implementation.
*   **`cmd/cli/main.go`:** The entry point for the interactive CLI application. It parses user input and executes the corresponding database commands.
*   **`pkg/brisedb/brise_test.go`:** Unit tests for the database functionality.

## Building and Running

### Building the project

To build the `brisedb` CLI, you can use the standard `go build` command:

```bash
go build -o brisedb cmd/cli/main.go
```

This will create an executable named `brisedb` in the current directory.

### Running the CLI

To run the interactive CLI, execute the compiled binary:

```bash
./brisedb
```

You can then issue commands to the database:

```
> SET name "John Doe"
> GET name
John Doe
> COUNT "John Doe"
1
> BEGIN
> SET name "Jane Doe"
> GET name
Jane Doe
> ROLLBACK
> GET name
John Doe
> STOP
```

### Running Tests

To run the tests, use the `go test` command:

```bash
go test ./pkg/brisedb/...
```

## Development Conventions

*   **Testing:** The project uses the standard Go `testing` package. All new functionality should be accompanied by unit tests.
*   **Coding Style:** The code follows standard Go formatting and conventions. Use `gofmt` to format your code.
*   **Dependencies:** The project has no external dependencies outside of the Go standard library.
