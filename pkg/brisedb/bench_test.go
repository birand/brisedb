package brisedb

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// ------------------------------------------------------------------ //
// Helpers
// ------------------------------------------------------------------ //

func newBenchDB(b *testing.B) (*BriseDB, *Session) {
	b.Helper()
	db, err := NewBriseDB(b.TempDir())
	if err != nil {
		b.Fatalf("NewBriseDB: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	return db, db.NewSession()
}

func newBenchDBWithCache(b *testing.B, cacheBytes uint64) (*BriseDB, *Session) {
	b.Helper()
	db, err := NewBriseDB(b.TempDir(), DBOptions{CacheSize: cacheBytes})
	if err != nil {
		b.Fatalf("NewBriseDB: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	return db, db.NewSession()
}

// ------------------------------------------------------------------ //
// Single-key operations
// ------------------------------------------------------------------ //

func BenchmarkSet(b *testing.B) {
	_, s := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Set(fmt.Sprintf("key:%d", i), "value")
	}
}

func BenchmarkGet(b *testing.B) {
	_, s := newBenchDB(b)
	s.Set("key", "value")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Get("key")
	}
}

func BenchmarkGetMiss(b *testing.B) {
	_, s := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Get("nosuchkey")
	}
}

func BenchmarkSetEX(b *testing.B) {
	_, s := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SetEX(fmt.Sprintf("key:%d", i), "value", time.Minute)
	}
}

func BenchmarkDelete(b *testing.B) {
	_, s := newBenchDB(b)
	const preload = 10_000
	for i := 0; i < preload; i++ {
		s.Set(fmt.Sprintf("key:%d", i), "value")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Delete(fmt.Sprintf("key:%d", i%preload))
	}
}

// ------------------------------------------------------------------ //
// Blob API — different payload sizes
// ------------------------------------------------------------------ //

var blobSizes = []struct {
	name string
	size int
}{
	{"1B", 1},
	{"1KB", 1 << 10},
	{"64KB", 64 << 10},
	{"1MB", 1 << 20},
}

func BenchmarkSetBlob(b *testing.B) {
	for _, tc := range blobSizes {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			_, s := newBenchDB(b)
			payload := bytes.Repeat([]byte("x"), tc.size)
			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.SetBlob(fmt.Sprintf("blob:%d", i), payload, 0)
			}
		})
	}
}

func BenchmarkSetBlobStream(b *testing.B) {
	for _, tc := range blobSizes {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			_, s := newBenchDB(b)
			payload := bytes.Repeat([]byte("x"), tc.size)
			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.SetBlobStream(fmt.Sprintf("blob:%d", i),
					bytes.NewReader(payload), int64(tc.size), 0)
			}
		})
	}
}

func BenchmarkGetBlob(b *testing.B) {
	for _, tc := range blobSizes {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			_, s := newBenchDB(b)
			payload := bytes.Repeat([]byte("x"), tc.size)
			s.SetBlob("blob", payload, 0)
			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				meta, ok := s.GetBlobMeta("blob")
				if !ok {
					b.Fatal("missing blob")
				}
				s.ReadBlobAt(meta.Addr, 0, -1)
			}
		})
	}
}

func BenchmarkGetBlobCached(b *testing.B) {
	for _, tc := range blobSizes {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			_, s := newBenchDBWithCache(b, 256<<20) // 256 MiB cache
			payload := bytes.Repeat([]byte("x"), tc.size)
			s.SetBlob("blob", payload, 0)
			// Warm the cache with one read before timing.
			meta, _ := s.GetBlobMeta("blob")
			s.ReadBlobAt(meta.Addr, 0, -1)
			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				meta, _ := s.GetBlobMeta("blob")
				s.ReadBlobAt(meta.Addr, 0, -1)
			}
		})
	}
}

// ------------------------------------------------------------------ //
// Transactions
// ------------------------------------------------------------------ //

func BenchmarkTransactionSingleKey(b *testing.B) {
	_, s := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.BeginTransaction()
		s.Set("k", "v")
		s.CommitTransaction()
	}
}

func BenchmarkTransactionTenKeys(b *testing.B) {
	_, s := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.BeginTransaction()
		for j := 0; j < 10; j++ {
			s.Set(fmt.Sprintf("k%d", j), "v")
		}
		s.CommitTransaction()
	}
}

func BenchmarkTransactionRollback(b *testing.B) {
	_, s := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.BeginTransaction()
		s.Set("k", "v")
		s.RollbackTransaction()
	}
}

func BenchmarkNestedTransaction(b *testing.B) {
	_, s := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.BeginTransaction()
		s.BeginTransaction()
		s.Set("k", "v")
		s.CommitTransaction()
		s.CommitTransaction()
	}
}

// ------------------------------------------------------------------ //
// Scans
// ------------------------------------------------------------------ //

func BenchmarkKeys(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			_, s := newBenchDB(b)
			for i := 0; i < n; i++ {
				s.Set(fmt.Sprintf("key:%d", i), "v")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.Keys("*")
			}
		})
	}
}

func BenchmarkScan(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			_, s := newBenchDB(b)
			for i := 0; i < n; i++ {
				s.Set(fmt.Sprintf("key:%d", i), "v")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.Scan(0, n)
			}
		})
	}
}

func BenchmarkCount(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			_, s := newBenchDB(b)
			for i := 0; i < n; i++ {
				s.Set(fmt.Sprintf("key:%d", i), "target")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.Count("target")
			}
		})
	}
}

// ------------------------------------------------------------------ //
// Parallel / concurrency
// ------------------------------------------------------------------ //

func BenchmarkSetParallel(b *testing.B) {
	db, _ := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := db.NewSession()
		i := 0
		for pb.Next() {
			s.Set(fmt.Sprintf("key:%d", i), "value")
			i++
		}
	})
}

func BenchmarkGetParallel(b *testing.B) {
	db, s := newBenchDB(b)
	s.Set("key", "value")
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		sess := db.NewSession()
		for pb.Next() {
			sess.Get("key")
		}
	})
}

// BenchmarkMixedReadWrite simulates a realistic 80% read / 20% write workload
// across multiple concurrent sessions.
func BenchmarkMixedReadWrite(b *testing.B) {
	db, s := newBenchDB(b)
	// Pre-populate 1000 keys so reads aren't all misses.
	for i := 0; i < 1000; i++ {
		s.Set(fmt.Sprintf("key:%d", i), "value")
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		sess := db.NewSession()
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key:%d", i%1000)
			if i%5 == 0 {
				sess.Set(key, "value")
			} else {
				sess.Get(key)
			}
			i++
		}
	})
}

// BenchmarkWALGroupCommit measures throughput when many goroutines write
// concurrently — the WAL flusher should batch their entries into fewer flushes.
func BenchmarkWALGroupCommit(b *testing.B) {
	db, _ := newBenchDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := db.NewSession()
		i := 0
		for pb.Next() {
			s.Set(fmt.Sprintf("walgc:%d", i), "v")
			i++
		}
	})
}

// ------------------------------------------------------------------ //
// Hash index micro-benchmarks
// ------------------------------------------------------------------ //

func BenchmarkHashIndexSet(b *testing.B) {
	hi, err := openHashIndex(b.TempDir()+"/index.hash", 0)
	if err != nil {
		b.Fatal(err)
	}
	defer hi.Close()
	addr := NeedleAddr{VolumeID: 1, Offset: 0, Size: 16}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hi.Set(fmt.Sprintf("key:%d", i), addr, 0)
	}
}

func BenchmarkHashIndexGet(b *testing.B) {
	hi, err := openHashIndex(b.TempDir()+"/index.hash", 0)
	if err != nil {
		b.Fatal(err)
	}
	defer hi.Close()
	addr := NeedleAddr{VolumeID: 1, Offset: 0, Size: 16}
	hi.Set("key", addr, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hi.Get("key")
	}
}

func BenchmarkHashIndexGetMiss(b *testing.B) {
	hi, err := openHashIndex(b.TempDir()+"/index.hash", 0)
	if err != nil {
		b.Fatal(err)
	}
	defer hi.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hi.Get("nosuchkey")
	}
}

func BenchmarkHashIndexForEach(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			hi, err := openHashIndex(b.TempDir()+"/index.hash", 0)
			if err != nil {
				b.Fatal(err)
			}
			defer hi.Close()
			addr := NeedleAddr{VolumeID: 1, Offset: 0, Size: 16}
			for i := 0; i < n; i++ {
				hi.Set(fmt.Sprintf("key:%d", i), addr, 0)
			}
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				hi.ForEach(now, func(_ string, _ NeedleAddr, _ int64) bool { return true })
			}
		})
	}
}

func BenchmarkHashIndexCompact(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			addr := NeedleAddr{VolumeID: 1, Offset: 0, Size: 16}
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				hi, err := openHashIndex(b.TempDir()+"/index.hash", 0)
				if err != nil {
					b.Fatal(err)
				}
				for j := 0; j < n; j++ {
					hi.Set(fmt.Sprintf("key:%d", j), addr, 0)
				}
				b.StartTimer()
				if err := hi.Compact(now); err != nil {
					b.Fatalf("Compact: %v", err)
				}
				b.StopTimer()
				hi.Close()
				b.StartTimer()
			}
		})
	}
}
