package client_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/birand/brisedb/pkg/brisedb"
	"github.com/birand/brisedb/pkg/client"
	"github.com/birand/brisedb/pkg/server"
)

func startPrimary(t *testing.T) (string, *brisedb.BriseDB) {
	t.Helper()
	db, err := brisedb.NewBriseDB(t.TempDir())
	if err != nil {
		t.Fatalf("primary NewBriseDB: %v", err)
	}
	srv, err := server.New(db, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("primary server.New: %v", err)
	}
	go srv.Start()
	t.Cleanup(func() { srv.Shutdown(); db.Close() })
	return srv.Addr(), db
}

func startReplica(t *testing.T, primaryAddr string) string {
	t.Helper()
	db, err := brisedb.NewBriseDB(t.TempDir())
	if err != nil {
		t.Fatalf("replica NewBriseDB: %v", err)
	}

	rc, err := client.DialReplica(primaryAddr, func(entry []byte) error {
		return db.ApplyReplicationEntry(entry)
	})
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}

	go rc.Stream(func(entry []byte) error {
		return db.ApplyReplicationEntry(entry)
	})

	srv, err := server.New(db, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("replica server.New: %v", err)
	}
	srv.ReadOnly = true
	go srv.Start()
	t.Cleanup(func() { rc.Close(); srv.Shutdown(); db.Close() })
	return srv.Addr()
}

func TestReplication_SnapshotAndLive(t *testing.T) {
	primaryAddr, _ := startPrimary(t)

	// Write some keys before the replica connects (snapshot test)
	pc := dial(t, primaryAddr)
	defer pc.Close()
	if err := pc.Set("snap1", "a"); err != nil {
		t.Fatalf("Set snap1: %v", err)
	}
	if err := pc.Set("snap2", "b"); err != nil {
		t.Fatalf("Set snap2: %v", err)
	}

	replicaAddr := startReplica(t, primaryAddr)
	time.Sleep(50 * time.Millisecond) // let snapshot apply

	rc := dial(t, replicaAddr)
	defer rc.Close()

	for _, tt := range []struct{ key, want string }{
		{"snap1", "a"},
		{"snap2", "b"},
	} {
		val, ok, err := rc.Get(tt.key)
		if err != nil {
			t.Fatalf("Get %s: %v", tt.key, err)
		}
		if !ok || val != tt.want {
			t.Errorf("replica Get %s: want %q, got %q (found=%v)", tt.key, tt.want, val, ok)
		}
	}

	// Write a live entry; replica should pick it up
	if err := pc.Set("live", "yes"); err != nil {
		t.Fatalf("Set live: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	val, ok, err := rc.Get("live")
	if err != nil {
		t.Fatalf("Get live: %v", err)
	}
	if !ok || val != "yes" {
		t.Errorf("replica live key: want yes, got %q (found=%v)", val, ok)
	}
}

func TestReplication_ReadOnly(t *testing.T) {
	primaryAddr, _ := startPrimary(t)
	replicaAddr := startReplica(t, primaryAddr)

	rc := dial(t, replicaAddr)
	defer rc.Close()

	if err := rc.Set("x", "1"); err == nil {
		t.Error("expected error writing to read-only replica, got nil")
	}
}

func TestReplication_MultipleReplicas(t *testing.T) {
	primaryAddr, _ := startPrimary(t)

	replicas := make([]string, 3)
	for i := range replicas {
		replicas[i] = startReplica(t, primaryAddr)
	}
	time.Sleep(50 * time.Millisecond)

	pc := dial(t, primaryAddr)
	defer pc.Close()

	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("k%d", i)
		val := fmt.Sprintf("v%d", i)
		if err := pc.Set(key, val); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}
	time.Sleep(50 * time.Millisecond)

	for ri, addr := range replicas {
		rc := dial(t, addr)
		for i := 0; i < 5; i++ {
			key := fmt.Sprintf("k%d", i)
			want := fmt.Sprintf("v%d", i)
			val, ok, err := rc.Get(key)
			if err != nil {
				t.Fatalf("replica %d Get %s: %v", ri, key, err)
			}
			if !ok || val != want {
				t.Errorf("replica %d Get %s: want %q, got %q", ri, key, want, val)
			}
		}
		rc.Close()
	}
}
