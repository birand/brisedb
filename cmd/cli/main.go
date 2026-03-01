package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/birand/brisedb/pkg/brisedb"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	db, err := brisedb.NewBriseDB("wal.log")
	if err != nil {
		logger.Error("failed to create database", "error", err)
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Printf("> ")
		input, _ := reader.ReadString('\n')
		command, args := parseCommand(input)

		if err := executeCommand(db, command, args); err != nil {
			logger.Error("command failed", "error", err)
		}
	}
}

func parseCommand(input string) (string, []string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nil
	}
	parts := strings.Fields(input)
	return parts[0], parts[1:]
}

func executeCommand(db *brisedb.BriseDB, command string, args []string) error {
	switch command {
	case "BEGIN":
		db.BeginTransaction()
	case "ROLLBACK":
		return db.RollbackTransaction()
	case "COMMIT":
		return db.CommitTransaction()
	case "SET":
		if len(args) < 2 {
			return fmt.Errorf("ERROR: Missing key or value argument for SET")
		}
		return db.Set(args[0], args[1])
	case "GET":
		if len(args) < 1 {
			return fmt.Errorf("ERROR: Missing key argument for GET")
		}
		if val, ok := db.Get(args[0]); ok {
			fmt.Println(val)
		} else {
			fmt.Printf("%s not set\n", args[0])
		}
	case "DELETE":
		if len(args) < 1 {
			return fmt.Errorf("ERROR: Missing key argument for DELETE")
		}
		return db.Delete(args[0])
	case "COUNT":
		if len(args) < 1 {
			return fmt.Errorf("ERROR: Missing value argument for COUNT")
		}
		fmt.Println(db.Count(args[0]))
	case "COMPACT":
		return db.Compact()
	case "STOP":
		os.Exit(0)
	case "":
		// Empty command, do nothing
	default:
		return fmt.Errorf("ERROR: Unrecognized Operation %s", command)
	}
	return nil
}
