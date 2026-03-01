package brisedb

import (
	"testing"
	"time"
)

func TestReplicationManager_FanoutAndRegister(t *testing.T) {
	rm := newReplicationManager()

	ch1 := make(chan []byte, 10)
	ch2 := make(chan []byte, 10)
	rm.Register(ch1)
	rm.Register(ch2)

	if rm.count() != 2 {
		t.Fatalf("want 2 replicas, got %d", rm.count())
	}

	rm.fanout([]byte("hello"))

	for i, ch := range []chan []byte{ch1, ch2} {
		select {
		case got := <-ch:
			if string(got) != "hello" {
				t.Errorf("ch%d: want hello, got %s", i+1, got)
			}
		default:
			t.Errorf("ch%d: no message received", i+1)
		}
	}
}

func TestReplicationManager_Unregister(t *testing.T) {
	rm := newReplicationManager()
	ch := make(chan []byte, 10)
	rm.Register(ch)
	rm.Unregister(ch)

	rm.fanout([]byte("msg"))
	select {
	case <-ch:
		t.Error("received message after Unregister")
	default:
	}
}

func TestReplicationManager_SlowReplicaDrop(t *testing.T) {
	rm := newReplicationManager()
	// channel with no buffer — always full
	ch := make(chan []byte)
	rm.Register(ch)

	// fanout must not block
	done := make(chan struct{})
	go func() {
		rm.fanout([]byte("data"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("fanout blocked on slow replica")
	}
}

func TestSnapshot(t *testing.T) {
	walPath := t.TempDir() + "/wal.log"
	db, err := NewBriseDB(walPath)
	if err != nil {
		t.Fatalf("NewBriseDB: %v", err)
	}
	defer db.Close()

	s := db.NewSession()
	s.Set("a", "1")
	s.Set("b", "2")
	s.SetEX("dead", "x", 50*time.Millisecond)
	time.Sleep(60 * time.Millisecond)

	entries := db.Snapshot()
	if len(entries) != 2 {
		t.Fatalf("snapshot: want 2 entries (excluding expired), got %d", len(entries))
	}
}

func TestApplyReplicationEntry(t *testing.T) {
	srcPath := t.TempDir() + "/src.wal"
	dstPath := t.TempDir() + "/dst.wal"

	src, err := NewBriseDB(srcPath)
	if err != nil {
		t.Fatalf("src NewBriseDB: %v", err)
	}
	defer src.Close()

	dst, err := NewBriseDB(dstPath)
	if err != nil {
		t.Fatalf("dst NewBriseDB: %v", err)
	}
	defer dst.Close()

	src.NewSession().Set("k", "v")

	for _, entry := range src.Snapshot() {
		if err := dst.ApplyReplicationEntry(entry); err != nil {
			t.Fatalf("ApplyReplicationEntry: %v", err)
		}
	}

	val, ok := dst.NewSession().Get("k")
	if !ok || val != "v" {
		t.Errorf("replica: want v, got %q (found=%v)", val, ok)
	}
}
