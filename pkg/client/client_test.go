package client_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/birand/brisedb/pkg/brisedb"
	"github.com/birand/brisedb/pkg/client"
	"github.com/birand/brisedb/pkg/server"
)

// startServer starts a server on a random port.
// Shutdown is registered via t.Cleanup so it runs after test defers.
func startServer(t *testing.T) string {
	t.Helper()
	db, err := brisedb.NewBriseDB(t.TempDir() + "/wal.log")
	if err != nil {
		t.Fatalf("NewBriseDB: %v", err)
	}
	srv, err := server.New(db, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	go srv.Start()
	t.Cleanup(func() {
		srv.Shutdown()
		db.Close()
	})
	return srv.Addr()
}

// dial opens a client connection. Caller must defer c.Close() so it runs
// before the server t.Cleanup shuts down (defers run before t.Cleanup).
func dial(t *testing.T, addr string) *client.Client {
	t.Helper()
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

func TestClientSetGet(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	if err := c.Set("name", "Alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, ok, err := c.Get("name")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || val != "Alice" {
		t.Errorf("Get: want (Alice, true), got (%q, %v)", val, ok)
	}
}

func TestClientGetMissing(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	_, ok, err := c.Get("ghost")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("expected not found, got found")
	}
}

func TestClientDelete(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	c.Set("x", "1")
	if err := c.Delete("x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, _ := c.Get("x")
	if ok {
		t.Error("key still exists after Delete")
	}
}

func TestClientCount(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	c.Set("a", "foo")
	c.Set("b", "foo")
	c.Set("c", "bar")

	n, err := c.Count("foo")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("Count foo: want 2, got %d", n)
	}
}

func TestClientTransaction_Commit(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	if err := c.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	c.Set("k", "v")
	if err := c.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	val, ok, _ := c.Get("k")
	if !ok || val != "v" {
		t.Errorf("after Commit: want (v, true), got (%q, %v)", val, ok)
	}
}

func TestClientTransaction_Rollback(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	c.Begin()
	c.Set("k", "v")
	if err := c.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	_, ok, _ := c.Get("k")
	if ok {
		t.Error("key visible after Rollback")
	}
}

func TestClientTransaction_IsolatedClients(t *testing.T) {
	addr := startServer(t)
	c1 := dial(t, addr)
	defer c1.Close()
	c2 := dial(t, addr)
	defer c2.Close()

	c1.Begin()
	c1.Set("iso", "yes")

	_, ok, _ := c2.Get("iso")
	if ok {
		t.Error("c2 saw uncommitted write from c1")
	}

	c1.Commit()

	val, ok, _ := c2.Get("iso")
	if !ok || val != "yes" {
		t.Errorf("c2 after commit: want yes, got %q (found=%v)", val, ok)
	}
}

func TestClientCommitWithNoTransaction(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	if err := c.Commit(); err == nil {
		t.Error("expected error committing with no transaction")
	}
}

func TestClientSetEX(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	if err := c.SetEX("temp", "val", 1*time.Second); err != nil {
		t.Fatalf("SetEX: %v", err)
	}

	_, ok, _ := c.Get("temp")
	if !ok {
		t.Fatal("key should exist before expiry")
	}

	time.Sleep(1100 * time.Millisecond)

	_, ok, _ = c.Get("temp")
	if ok {
		t.Error("key should be expired")
	}
}

func TestClientTTL(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	// Missing key
	n, err := c.TTL("ghost")
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if n != -2 {
		t.Errorf("missing key TTL: want -2, got %d", n)
	}

	// Key with no expiry
	c.Set("persist", "yes")
	n, _ = c.TTL("persist")
	if n != -1 {
		t.Errorf("no-expiry TTL: want -1, got %d", n)
	}

	// Key with TTL
	c.SetEX("temp", "x", 10*time.Second)
	n, _ = c.TTL("temp")
	if n <= 0 || n > 10 {
		t.Errorf("TTL with expiry: want 1-10, got %d", n)
	}
}

func TestClientPersist(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	c.SetEX("k", "v", 10*time.Second)

	removed, err := c.Persist("k")
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if !removed {
		t.Error("Persist: expected true (TTL was removed)")
	}

	n, _ := c.TTL("k")
	if n != -1 {
		t.Errorf("after Persist: want -1, got %d", n)
	}
}

func TestClientCompact(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	c.Set("a", "1")
	c.Set("b", "2")
	c.Delete("a")

	if err := c.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Data still readable after compaction
	_, ok, _ := c.Get("a")
	if ok {
		t.Error("deleted key 'a' still present after Compact")
	}
	val, ok, _ := c.Get("b")
	if !ok || val != "2" {
		t.Errorf("key 'b' after Compact: want (2, true), got (%q, %v)", val, ok)
	}
}

func TestClientConcurrent(t *testing.T) {
	addr := startServer(t)

	const n = 20
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			c := dial(t, addr)
			defer c.Close()
			key := fmt.Sprintf("key%d", i)
			val := fmt.Sprintf("val%d", i)
			if err := c.Set(key, val); err != nil {
				errs <- fmt.Errorf("Set %s: %w", key, err)
				return
			}
			got, ok, err := c.Get(key)
			if err != nil {
				errs <- fmt.Errorf("Get %s: %w", key, err)
				return
			}
			if !ok || got != val {
				errs <- fmt.Errorf("Get %s: want %s, got %q (found=%v)", key, val, got, ok)
				return
			}
			errs <- nil
		}(i)
	}

	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
