package brisedb

import (
	"fmt"
	"testing"
	"time"
)

func setupTestDB(t *testing.T) (*BriseDB, *Session, func()) {
	walPath := t.TempDir() + "/wal.log"

	db, err := NewBriseDB(walPath)
	if err != nil {
		t.Fatalf("Failed to create BriseDB: %v", err)
	}

	session := db.NewSession()

	return db, session, func() {
		db.walFile.Close()
	}
}

func TestBriseDB(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	t.Run("SET and GET", func(t *testing.T) {
		testCases := []struct {
			key   string
			value string
		}{
			{"name", "John Doe"},
			{"age", "30"},
			{"city", "New York"},
		}

		for _, tc := range testCases {
			session.Set(tc.key, tc.value)
			value, ok := session.Get(tc.key)
			if !ok || value != tc.value {
				t.Errorf("SET/GET operation failed: key=%s, expected=%s, got=%s", tc.key, tc.value, value)
			}
		}
	})

	t.Run("DELETE", func(t *testing.T) {
		session.Set("todelete", "somevalue")
		session.Delete("todelete")
		_, ok := session.Get("todelete")
		if ok {
			t.Error("DELETE operation failed: key 'todelete' should have been deleted")
		}
	})

	t.Run("COUNT", func(t *testing.T) {
		session.Set("key1", "value1")
		session.Set("key2", "value1")
		session.Set("key3", "value2")

		count := session.Count("value1")
		if count != 2 {
			t.Errorf("COUNT operation failed: expected 2, got %d", count)
		}

		count = session.Count("value2")
		if count != 1 {
			t.Errorf("COUNT operation failed: expected 1, got %d", count)
		}

		count = session.Count("value3")
		if count != 0 {
			t.Errorf("COUNT operation failed: expected 0, got %d", count)
		}
	})
}

func TestTransaction(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	t.Run("Commit", func(t *testing.T) {
		session.BeginTransaction()
		session.Set("a", "10")
		if err := session.CommitTransaction(); err != nil {
			t.Errorf("CommitTransaction failed: %v", err)
		}
		val, ok := session.Get("a")
		if !ok || val != "10" {
			t.Error("CommitTransaction failed: value not committed")
		}
	})

	t.Run("Rollback", func(t *testing.T) {
		session.BeginTransaction()
		session.Set("b", "20")
		if err := session.RollbackTransaction(); err != nil {
			t.Errorf("RollbackTransaction failed: %v", err)
		}
		_, ok := session.Get("b")
		if ok {
			t.Error("RollbackTransaction failed: value not rolled back")
		}
	})

	t.Run("Nested Transactions", func(t *testing.T) {
		session.BeginTransaction()
		session.Set("c", "30")
		session.BeginTransaction()
		session.Set("d", "40")
		session.CommitTransaction()
		val, ok := session.Get("d")
		if !ok || val != "40" {
			t.Error("Nested transaction commit failed")
		}
		session.RollbackTransaction()
		_, ok = session.Get("c")
		if ok {
			t.Error("Nested transaction rollback failed")
		}
	})
}

func TestPersistence(t *testing.T) {
	walPath := t.TempDir() + "/wal.log"

	db, err := NewBriseDB(walPath)
	if err != nil {
		t.Fatalf("Failed to create BriseDB: %v", err)
	}
	db.NewSession().Set("persisted", "true")
	db.walFile.Close()

	db2, err := NewBriseDB(walPath)
	if err != nil {
		t.Fatalf("Failed to create BriseDB: %v", err)
	}
	defer db2.walFile.Close()

	val, ok := db2.NewSession().Get("persisted")
	if !ok || val != "true" {
		t.Error("Persistence failed: value not replayed from WAL")
	}
}

func TestDeleteInTransaction(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	session.Set("key", "value")

	session.BeginTransaction()
	session.Delete("key")
	val, ok := session.Get("key")
	if ok {
		t.Errorf("DELETE in transaction should hide key, got %q", val)
	}
	session.CommitTransaction()

	_, ok = session.Get("key")
	if ok {
		t.Error("DELETE committed but key still visible")
	}
}

func TestCountInTransaction(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	session.Set("a", "foo")
	session.Set("b", "foo")

	session.BeginTransaction()
	session.Set("c", "foo")
	session.Delete("a")

	count := session.Count("foo")
	if count != 2 {
		t.Errorf("COUNT in transaction: expected 2, got %d", count)
	}

	session.RollbackTransaction()
}

func TestKeys(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	session.Set("foo", "1")
	session.Set("bar", "2")
	session.Set("baz", "3")
	session.Set("qux", "4")

	t.Run("star matches all", func(t *testing.T) {
		keys, err := session.Keys("*")
		if err != nil {
			t.Fatalf("Keys: %v", err)
		}
		if len(keys) != 4 {
			t.Errorf("want 4 keys, got %d: %v", len(keys), keys)
		}
	})

	t.Run("prefix match", func(t *testing.T) {
		keys, _ := session.Keys("ba*")
		if len(keys) != 2 {
			t.Errorf("ba*: want 2, got %d: %v", len(keys), keys)
		}
	})

	t.Run("exact match", func(t *testing.T) {
		keys, _ := session.Keys("foo")
		if len(keys) != 1 || keys[0] != "foo" {
			t.Errorf("exact: want [foo], got %v", keys)
		}
	})

	t.Run("question mark", func(t *testing.T) {
		keys, _ := session.Keys("ba?")
		if len(keys) != 2 {
			t.Errorf("ba?: want 2, got %d: %v", len(keys), keys)
		}
	})

	t.Run("no match", func(t *testing.T) {
		keys, _ := session.Keys("xyz*")
		if len(keys) != 0 {
			t.Errorf("no match: want 0, got %d", len(keys))
		}
	})

	t.Run("sorted result", func(t *testing.T) {
		keys, _ := session.Keys("*")
		for i := 1; i < len(keys); i++ {
			if keys[i] < keys[i-1] {
				t.Errorf("not sorted: %v", keys)
			}
		}
	})
}

func TestKeys_ExcludesExpired(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	session.Set("live", "1")
	session.SetEX("dead", "2", 50*time.Millisecond)

	time.Sleep(60 * time.Millisecond)

	keys, _ := session.Keys("*")
	for _, k := range keys {
		if k == "dead" {
			t.Error("Keys should exclude expired keys")
		}
	}
}

func TestScan(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	for i := 0; i < 10; i++ {
		session.Set(fmt.Sprintf("key%02d", i), "v")
	}

	t.Run("full scan", func(t *testing.T) {
		collected := map[string]bool{}
		cursor := 0
		for {
			next, keys := session.Scan(cursor, 3)
			for _, k := range keys {
				collected[k] = true
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		if len(collected) != 10 {
			t.Errorf("scan collected %d keys, want 10", len(collected))
		}
	})

	t.Run("count larger than total", func(t *testing.T) {
		next, keys := session.Scan(0, 100)
		if next != 0 {
			t.Errorf("want cursor 0 (done), got %d", next)
		}
		if len(keys) != 10 {
			t.Errorf("want 10 keys, got %d", len(keys))
		}
	})

	t.Run("cursor past end", func(t *testing.T) {
		next, keys := session.Scan(999, 10)
		if next != 0 || len(keys) != 0 {
			t.Errorf("past end: want (0, []), got (%d, %v)", next, keys)
		}
	})
}

func TestTTL_BasicExpiry(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	session.SetEX("k", "v", 50*time.Millisecond)

	// Key is visible before expiry
	val, ok := session.Get("k")
	if !ok || val != "v" {
		t.Fatalf("expected key before expiry, got (%q, %v)", val, ok)
	}

	time.Sleep(60 * time.Millisecond)

	// Lazy expiry: Get should report the key as absent
	_, ok = session.Get("k")
	if ok {
		t.Error("expected key to be expired, but Get returned true")
	}
}

func TestTTL_TTLMethod(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	// Key not found → -2
	if got := session.TTL("missing"); got != -2 {
		t.Errorf("TTL missing key: want -2, got %d", got)
	}

	// Key with no expiry → -1
	session.Set("persist", "yes")
	if got := session.TTL("persist"); got != -1 {
		t.Errorf("TTL no-expiry key: want -1, got %d", got)
	}

	// Key with TTL → positive seconds
	session.SetEX("temp", "x", 10*time.Second)
	ttl := session.TTL("temp")
	if ttl <= 0 || ttl > 10 {
		t.Errorf("TTL with expiry: want 1-10, got %d", ttl)
	}
}

func TestTTL_Persist(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	session.SetEX("k", "v", 10*time.Second)
	if got := session.TTL("k"); got <= 0 {
		t.Fatalf("expected positive TTL, got %d", got)
	}

	session.Persist("k")

	if got := session.TTL("k"); got != -1 {
		t.Errorf("after Persist: want -1, got %d", got)
	}
}

func TestTTL_SetClearsTTL(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	session.SetEX("k", "v1", 10*time.Second)
	session.Set("k", "v2") // plain Set should clear the TTL

	if got := session.TTL("k"); got != -1 {
		t.Errorf("Set should clear TTL: want -1, got %d", got)
	}
}

func TestTTL_WALPersistence(t *testing.T) {
	walPath := t.TempDir() + "/wal.log"

	db, err := NewBriseDB(walPath)
	if err != nil {
		t.Fatalf("NewBriseDB: %v", err)
	}
	s := db.NewSession()
	s.SetEX("alive", "yes", 10*time.Second)
	s.SetEX("dead", "no", 1*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	db.Close()

	db2, err := NewBriseDB(walPath)
	if err != nil {
		t.Fatalf("NewBriseDB reload: %v", err)
	}
	defer db2.Close()
	s2 := db2.NewSession()

	if val, ok := s2.Get("alive"); !ok || val != "yes" {
		t.Errorf("non-expired key after reload: want yes, got (%q, %v)", val, ok)
	}
	if _, ok := s2.Get("dead"); ok {
		t.Error("expired key should not be present after reload")
	}
}

func TestTTL_CountExcludesExpired(t *testing.T) {
	_, session, cleanup := setupTestDB(t)
	defer cleanup()

	session.Set("a", "foo")
	session.SetEX("b", "foo", 50*time.Millisecond)

	if got := session.Count("foo"); got != 2 {
		t.Fatalf("before expiry: want 2, got %d", got)
	}

	time.Sleep(60 * time.Millisecond)

	if got := session.Count("foo"); got != 1 {
		t.Errorf("after expiry: want 1, got %d", got)
	}
}
