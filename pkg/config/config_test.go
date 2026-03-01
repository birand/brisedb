package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "brisedb.json")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load("", false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":6380" {
		t.Errorf("default Addr: want :6380, got %q", cfg.Addr)
	}
	if cfg.WAL != "wal.log" {
		t.Errorf("default WAL: want wal.log, got %q", cfg.WAL)
	}
}

func TestLoad_FromFile(t *testing.T) {
	p := writeConfig(t, `{"addr":":9999","wal":"/tmp/test.wal"}`)
	cfg, err := Load(p, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("Addr: want :9999, got %q", cfg.Addr)
	}
	if cfg.WAL != "/tmp/test.wal" {
		t.Errorf("WAL: want /tmp/test.wal, got %q", cfg.WAL)
	}
}

func TestLoad_PartialFile_KeepsDefaults(t *testing.T) {
	// Only addr is set; wal should fall back to default
	p := writeConfig(t, `{"addr":":7777"}`)
	cfg, err := Load(p, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":7777" {
		t.Errorf("Addr: want :7777, got %q", cfg.Addr)
	}
	if cfg.WAL != "wal.log" {
		t.Errorf("WAL should be default, got %q", cfg.WAL)
	}
}

func TestLoad_MissingFile_Explicit_ReturnsError(t *testing.T) {
	_, err := Load("/nonexistent/brisedb.json", true)
	if err == nil {
		t.Error("expected error for explicit missing config file")
	}
}

func TestLoad_MissingFile_Implicit_ReturnsDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/brisedb.json", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Addr != ":6380" {
		t.Errorf("expected default addr, got %q", cfg.Addr)
	}
}

func TestLoad_InvalidJSON_ReturnsError(t *testing.T) {
	p := writeConfig(t, `{not valid json}`)
	_, err := Load(p, true)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestApply_FlagsOverrideConfig(t *testing.T) {
	p := writeConfig(t, `{"addr":":9999","wal":"/config.wal"}`)
	cfg, _ := Load(p, true)

	cfg.Apply(":1111", "/flag.wal")

	if cfg.Addr != ":1111" {
		t.Errorf("Addr: want :1111, got %q", cfg.Addr)
	}
	if cfg.WAL != "/flag.wal" {
		t.Errorf("WAL: want /flag.wal, got %q", cfg.WAL)
	}
}

func TestApply_EmptyFlagsKeepConfig(t *testing.T) {
	p := writeConfig(t, `{"addr":":9999","wal":"/config.wal"}`)
	cfg, _ := Load(p, true)

	cfg.Apply("", "")

	if cfg.Addr != ":9999" {
		t.Errorf("Addr should not change, got %q", cfg.Addr)
	}
	if cfg.WAL != "/config.wal" {
		t.Errorf("WAL should not change, got %q", cfg.WAL)
	}
}
