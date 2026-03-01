package brisedb

import (
	"testing"
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
