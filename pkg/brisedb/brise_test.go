package brisedb

import (
	"os"
	"testing"
)

func setupTestDB(t *testing.T) (*BriseDB, func()) {
	// Truncate the WAL file
	walFile, err := os.OpenFile("wal.log", os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("Failed to truncate WAL file: %v", err)
	}
	walFile.Close()

	db, err := NewBriseDB()
	if err != nil {
		t.Fatalf("Failed to create BriseDB: %v", err)
	}

	return db, func() {
		db.walFile.Close()
	}
}

func TestBriseDB(t *testing.T) {
	db, cleanup := setupTestDB(t)
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
			db.Set(tc.key, tc.value)
			value, ok := db.Get(tc.key)
			if !ok || value != tc.value {
				t.Errorf("SET/GET operation failed: key=%s, expected=%s, got=%s", tc.key, tc.value, value)
			}
		}
	})

	t.Run("DELETE", func(t *testing.T) {
		db.Set("todelete", "somevalue")
		db.Delete("todelete")
		_, ok := db.Get("todelete")
		if ok {
			t.Error("DELETE operation failed: key 'todelete' should have been deleted")
		}
	})

	t.Run("COUNT", func(t *testing.T) {
		db.Set("key1", "value1")
		db.Set("key2", "value1")
		db.Set("key3", "value2")

		count := db.Count("value1")
		if count != 2 {
			t.Errorf("COUNT operation failed: expected 2, got %d", count)
		}

		count = db.Count("value2")
		if count != 1 {
			t.Errorf("COUNT operation failed: expected 1, got %d", count)
		}

		count = db.Count("value3")
		if count != 0 {
			t.Errorf("COUNT operation failed: expected 0, got %d", count)
		}
	})
}

func TestTransaction(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	t.Run("Commit", func(t *testing.T) {
		db.BeginTransaction()
		db.Set("a", "10")
		if err := db.CommitTransaction(); err != nil {
			t.Errorf("CommitTransaction failed: %v", err)
		}
		val, ok := db.Get("a")
		if !ok || val != "10" {
			t.Error("CommitTransaction failed: value not committed")
		}
	})

	t.Run("Rollback", func(t *testing.T) {
		db.BeginTransaction()
		db.Set("b", "20")
		if err := db.RollbackTransaction(); err != nil {
			t.Errorf("RollbackTransaction failed: %v", err)
		}
		_, ok := db.Get("b")
		if ok {
			t.Error("RollbackTransaction failed: value not rolled back")
		}
	})

	t.Run("Nested Transactions", func(t *testing.T) {
		db.BeginTransaction()
		db.Set("c", "30")
		db.BeginTransaction()
		db.Set("d", "40")
		db.CommitTransaction()
		val, ok := db.Get("d")
		if !ok || val != "40" {
			t.Error("Nested transaction commit failed")
		}
		db.RollbackTransaction()
		_, ok = db.Get("c")
		if ok {
			t.Error("Nested transaction rollback failed")
		}
	})
}

func TestPersistence(t *testing.T) {
	db, cleanup := setupTestDB(t)

	db.Set("persisted", "true")

	cleanup()

	db2, err := NewBriseDB()
	if err != nil {
		t.Fatalf("Failed to create BriseDB: %v", err)
	}
	defer db2.walFile.Close()

	val, ok := db2.Get("persisted")
	if !ok || val != "true" {
		t.Error("Persistence failed: value not replayed from WAL")
	}
}