package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/birand/brisedb/pkg/brisedb"
	"github.com/birand/brisedb/pkg/config"
	"github.com/birand/brisedb/pkg/server"
)

func main() {
	configPath := flag.String("config", "", "path to JSON config file (optional)")
	addr := flag.String("addr", "", "TCP address to listen on (overrides config)")
	walPath := flag.String("wal", "", "path to WAL file (overrides config)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Load config file; explicit=true when --config was provided so a missing
	// file is treated as an error rather than silently ignored.
	explicit := isFlagSet("config")
	cfg, err := config.Load(*configPath, explicit)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Flags override config values
	cfg.Apply(*addr, *walPath)

	logger.Info("starting brisedb", "addr", cfg.Addr, "wal", cfg.WAL)

	db, err := brisedb.NewBriseDB(cfg.WAL)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}

	srv, err := server.New(db, cfg.Addr)
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

// isFlagSet reports whether the named flag was explicitly provided on the
// command line.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
