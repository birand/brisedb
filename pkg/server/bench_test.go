package server

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/birand/brisedb/pkg/brisedb"
)

// benchConn dials the server and returns send/recv helpers.
// The connection is closed via b.Cleanup.
func benchConn(b *testing.B, addr string) func(cmd string) string {
	b.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	r := bufio.NewReader(conn)
	b.Cleanup(func() { conn.Close() })

	return func(cmd string) string {
		fmt.Fprintf(conn, "%s\n", cmd)
		line, err := r.ReadString('\n')
		if err != nil {
			b.Fatalf("read: %v", err)
		}
		return strings.TrimRight(line, "\n")
	}
}

func startBenchServer(b *testing.B) string {
	b.Helper()
	db, err := brisedb.NewBriseDB(b.TempDir() + "/wal.log")
	if err != nil {
		b.Fatalf("NewBriseDB: %v", err)
	}
	srv, err := New(db, "127.0.0.1:0")
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	go srv.Start()
	b.Cleanup(func() {
		srv.Shutdown()
		db.Close()
	})
	return srv.Addr()
}

func BenchmarkServerSet(b *testing.B) {
	addr := startBenchServer(b)
	send := benchConn(b, addr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		send(fmt.Sprintf("SET key%d value", i))
	}
}

func BenchmarkServerGet(b *testing.B) {
	addr := startBenchServer(b)
	send := benchConn(b, addr)
	send("SET key value")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		send("GET key")
	}
}

func BenchmarkServerGetMiss(b *testing.B) {
	addr := startBenchServer(b)
	send := benchConn(b, addr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		send("GET missing")
	}
}

func BenchmarkServerDelete(b *testing.B) {
	addr := startBenchServer(b)
	send := benchConn(b, addr)
	for i := 0; i < b.N; i++ {
		send(fmt.Sprintf("SET key%d value", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		send(fmt.Sprintf("DELETE key%d", i))
	}
}

func BenchmarkServerTransaction(b *testing.B) {
	addr := startBenchServer(b)
	send := benchConn(b, addr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		send("BEGIN")
		send(fmt.Sprintf("SET k%d v", i))
		send("COMMIT")
	}
}

func BenchmarkServerSetParallel(b *testing.B) {
	addr := startBenchServer(b)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		send := benchConn(b, addr)
		i := 0
		for pb.Next() {
			send(fmt.Sprintf("SET key%d value", i))
			i++
		}
	})
}

func BenchmarkServerGetParallel(b *testing.B) {
	addr := startBenchServer(b)
	send0 := benchConn(b, addr)
	send0("SET key value")
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		send := benchConn(b, addr)
		for pb.Next() {
			send("GET key")
		}
	})
}

func BenchmarkServerMixedReadWrite(b *testing.B) {
	addr := startBenchServer(b)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		send := benchConn(b, addr)
		send("SET key value")
		i := 0
		for pb.Next() {
			if i%5 == 0 {
				send("SET key value")
			} else {
				send("GET key")
			}
			i++
		}
	})
}
