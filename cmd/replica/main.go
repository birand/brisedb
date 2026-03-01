// cmd/replica starts a read-only brisedb server that streams WAL entries from
// a primary server.
package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/birand/brisedb/pkg/brisedb"
	"github.com/birand/brisedb/pkg/client"
	"github.com/birand/brisedb/pkg/server"
)

func main() {
	primary := flag.String("primary", "localhost:6380", "address of the primary brisedb server")
	addr := flag.String("addr", ":6381", "TCP address for this replica to listen on")
	walPath := flag.String("wal", "replica.wal", "path to replica WAL file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	db, err := brisedb.NewBriseDB(*walPath)
	if err != nil {
		logger.Error("failed to open replica database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	logger.Info("connecting to primary", "primary", *primary)

	rc, err := client.DialReplica(*primary, func(entry []byte) error {
		return db.ApplyReplicationEntry(entry)
	})
	if err != nil {
		logger.Error("failed to connect to primary", "error", err)
		os.Exit(1)
	}
	defer rc.Close()

	logger.Info("snapshot applied, streaming live entries")

	// Stream live entries in the background.
	go func() {
		if err := rc.Stream(func(entry []byte) error {
			return db.ApplyReplicationEntry(entry)
		}); err != nil {
			logger.Warn("replication stream ended", "error", err)
		}
	}()

	srv, err := server.New(db, *addr)
	if err != nil {
		logger.Error("failed to start replica server", "error", err)
		os.Exit(1)
	}
	srv.ReadOnly = true

	logger.Info("replica listening", "addr", srv.Addr())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("shutting down replica")
		srv.Shutdown()
	}()

	srv.Start()
}
