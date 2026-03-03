// Package config loads brisedb server configuration from a JSON file.
// Command-line flags always take precedence over file values.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Config holds all server settings.
type Config struct {
	Addr          string   `json:"addr"`            // TCP listen address (default ":6380")
	DataDir       string   `json:"data_dir"`        // primary database directory
	Drives        []string `json:"drives"`          // additional storage drives
	MaxVolumeSize uint64   `json:"max_volume_size"` // bytes per volume, 0 = 2 GiB
	VolumeServers     []string `json:"volume_servers"`     // remote HTTP volume server URLs
	ReplicationFactor int      `json:"replication_factor"` // blob copies per write (0/1 = no replication)
	HTTPAddr          string   `json:"http_addr"`          // HTTP blob API address (e.g. ":6381"); empty = disabled
}

// defaults returns a Config pre-filled with default values.
func defaults() Config {
	return Config{
		Addr:    ":6380",
		DataDir: "brisedb-data",
	}
}

// Load reads a JSON config file from path and returns the result merged with
// defaults. If path is empty the defaults are returned. If the file does not
// exist and path was not explicitly provided, defaults are returned silently.
func Load(path string, explicit bool) (Config, error) {
	cfg := defaults()
	if path == "" {
		return cfg, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			// Config file not found but not explicitly requested — use defaults.
			return cfg, nil
		}
		return cfg, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

// Apply overlays non-zero flag values on top of cfg.
func (c *Config) Apply(addr, dataDir string) {
	if addr != "" {
		c.Addr = addr
	}
	if dataDir != "" {
		c.DataDir = dataDir
	}
}
