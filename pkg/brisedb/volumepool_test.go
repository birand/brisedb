package brisedb

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/birand/brisedb/pkg/volumeserver"
)

// newTestPool creates a VolumePool with a local VolumeManager backed by a temp dir.
func newTestPool(t *testing.T, remotes ...*volumeserver.Client) *VolumePool {
	t.Helper()
	vm := newTestVM(t, 0)
	pool, err := newVolumePool(vm, remotes, t.TempDir())
	if err != nil {
		t.Fatalf("newVolumePool: %v", err)
	}
	return pool
}

func TestVolumePool_LocalWriteRead(t *testing.T) {
	pool := newTestPool(t)

	data := []byte("local blob")
	addr, err := pool.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := pool.Read(addr)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("want %q, got %q", data, got)
	}
}

func TestVolumePool_RemoteWriteRead(t *testing.T) {
	// Start an in-process HTTP volume server backed by a local VolumeManager.
	remoteVM := newTestVM(t, 0)
	backend := &volumeserver.FuncBackend{
		WriteFn: func(data []byte) (volumeserver.WriteResult, error) {
			addr, err := remoteVM.Write(data)
			if err != nil {
				return volumeserver.WriteResult{}, err
			}
			return volumeserver.WriteResult{VolumeID: addr.VolumeID, Offset: addr.Offset, Size: addr.Size}, nil
		},
		ReadFn: func(volID uint32, offset, size uint64) ([]byte, error) {
			return remoteVM.Read(NeedleAddr{VolumeID: volID, Offset: offset, Size: size})
		},
		StatsFn: func() []volumeserver.VolumeStat { return nil },
	}
	srv := volumeserver.New(backend, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := volumeserver.NewClient(ts.URL)
	pool := newTestPool(t, client)

	// Force write to go to remote (pool has 2 backends: local + remote, idx alternates).
	// Write twice so we get at least one remote write.
	var addrs []NeedleAddr
	for i := 0; i < 4; i++ {
		data := []byte(fmt.Sprintf("blob-%d", i))
		addr, err := pool.Write(data)
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		addrs = append(addrs, addr)
	}

	// All writes should be readable regardless of which backend they went to.
	for i, addr := range addrs {
		got, err := pool.Read(addr)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		want := fmt.Sprintf("blob-%d", i)
		if string(got) != want {
			t.Errorf("entry %d: want %q, got %q", i, want, got)
		}
	}
}

func TestVolumePool_RegistryPersistence(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVM(t, 0)

	pool, err := newVolumePool(vm, nil, dir)
	if err != nil {
		t.Fatalf("newVolumePool: %v", err)
	}

	data := []byte("persist-me")
	addr, err := pool.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Reload the pool from the same dir — registry should be restored.
	pool2, err := newVolumePool(vm, nil, dir)
	if err != nil {
		t.Fatalf("reload newVolumePool: %v", err)
	}

	got, err := pool2.Read(addr)
	if err != nil {
		t.Fatalf("Read after reload: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("want %q, got %q", data, got)
	}
}
