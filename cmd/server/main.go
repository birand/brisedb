package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/birand/brisedb/pkg/brisedb"
	"github.com/birand/brisedb/pkg/server"
)

func main() {
	addr := flag.String("addr", ":6380", "TCP address to listen on")
	walPath := flag.String("wal", "wal.log", "Path to WAL file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	db, err := brisedb.NewBriseDB(*walPath)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}

	srv, err := server.New(db, *addr)
	if err != nil {
		logger.Error("failed to start server", "error", err)
		os.Exit(1)
	}

	logger.Info("brisedb server listening", "addr", srv.Addr())

	// Handle SIGINT/SIGTERM for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("shutting down")
		srv.Shutdown()
	}()

	if err := srv.Start(); err != nil {
		logger.Error("server error", "error", err)
	}

	if err := db.Compact(); err != nil {
		logger.Error("compact failed", "error", err)
	}
	db.Close()
}
