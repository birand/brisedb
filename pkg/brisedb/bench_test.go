package brisedb

import (
	"fmt"
	"testing"
	"time"
)

func setupBenchDB(b *testing.B) (*BriseDB, *Session) {
	b.Helper()
	db, err := NewBriseDB(b.TempDir() + "/wal.log")
	if err != nil {
		b.Fatalf("NewBriseDB: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	return db, db.NewSession()
}

func BenchmarkSet(b *testing.B) {
	_, s := setupBenchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Set(fmt.Sprintf("key%d", i), "value")
	}
}

func BenchmarkGet(b *testing.B) {
	_, s := setupBenchDB(b)
	s.Set("key", "value")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Get("key")
	}
}

func BenchmarkGetMiss(b *testing.B) {
	_, s := setupBenchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Get("missing")
	}
}

func BenchmarkDelete(b *testing.B) {
	_, s := setupBenchDB(b)
	// Pre-populate
	for i := 0; i < b.N; i++ {
		s.Set(fmt.Sprintf("key%d", i), "value")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Delete(fmt.Sprintf("key%d", i))
	}
}

func BenchmarkCount(b *testing.B) {
	_, s := setupBenchDB(b)
	for i := 0; i < 1000; i++ {
		s.Set(fmt.Sprintf("key%d", i), "foo")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Count("foo")
	}
}

func BenchmarkSetEX(b *testing.B) {
	_, s := setupBenchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SetEX(fmt.Sprintf("key%d", i), "value", time.Minute)
	}
}

func BenchmarkTransaction(b *testing.B) {
	_, s := setupBenchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.BeginTransaction()
		s.Set("k", "v")
		s.CommitTransaction()
	}
}

func BenchmarkNestedTransaction(b *testing.B) {
	_, s := setupBenchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.BeginTransaction()
		s.BeginTransaction()
		s.Set("k", "v")
		s.CommitTransaction() // inner → outer
		s.CommitTransaction() // outer → store
	}
}

func BenchmarkSetParallel(b *testing.B) {
	db, _ := setupBenchDB(b)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := db.NewSession()
		i := 0
		for pb.Next() {
			s.Set(fmt.Sprintf("key%d", i), "value")
			i++
		}
	})
}

func BenchmarkGetParallel(b *testing.B) {
	db, s := setupBenchDB(b)
	s.Set("key", "value")
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		sess := db.NewSession()
		for pb.Next() {
			sess.Get("key")
		}
	})
}

func BenchmarkMixedReadWrite(b *testing.B) {
	db, _ := setupBenchDB(b)
	// 80% reads, 20% writes
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := db.NewSession()
		s.Set("key", "value")
		i := 0
		for pb.Next() {
			if i%5 == 0 {
				s.Set("key", "value")
			} else {
				s.Get("key")
			}
			i++
		}
	})
}
