package brisedb

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/birand/brisedb/pkg/volumeserver"
)

// newTestPool creates a VolumePool backed by a temp-dir VolumeManager.
func newTestPool(t *testing.T, rf int, remotes ...*volumeserver.Client) *VolumePool {
	t.Helper()
	vm := newTestVM(t, 0)
	pool, err := newVolumePool(vm, remotes, t.TempDir(), rf, 0)
	if err != nil {
		t.Fatalf("newVolumePool: %v", err)
	}
	return pool
}

// newTestVolumeServer starts an httptest server wrapping a fresh VolumeManager.
func newTestVolumeServer(t *testing.T) (*httptest.Server, *VolumeManager) {
	t.Helper()
	vm := newTestVM(t, 0)
	backend := &volumeserver.FuncBackend{
		WriteFn: func(data []byte) (volumeserver.WriteResult, error) {
			addr, err := vm.Write(data)
			return volumeserver.WriteResult{VolumeID: addr.VolumeID, Offset: addr.Offset, Size: addr.Size}, err
		},
		WriteToVolumeFn: func(id uint32, data []byte) (volumeserver.WriteResult, error) {
			addr, err := vm.WriteToVolume(id, data)
			return volumeserver.WriteResult{VolumeID: addr.VolumeID, Offset: addr.Offset, Size: addr.Size}, err
		},
		ReadFn: func(volID uint32, offset, size uint64) ([]byte, error) {
			return vm.Read(NeedleAddr{VolumeID: volID, Offset: offset, Size: size})
		},
		StatsFn: func() []volumeserver.VolumeStat { return nil },
	}
	ts := httptest.NewServer(volumeserver.New(backend, nil).Handler())
	t.Cleanup(ts.Close)
	return ts, vm
}

func TestVolumePool_LocalWriteRead(t *testing.T) {
	pool := newTestPool(t, 1)

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
	ts, _ := newTestVolumeServer(t)
	pool := newTestPool(t, 1, volumeserver.NewClient(ts.URL))

	var addrs []NeedleAddr
	for i := 0; i < 4; i++ {
		data := []byte(fmt.Sprintf("blob-%d", i))
		addr, err := pool.Write(data)
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		addrs = append(addrs, addr)
	}

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

func TestVolumePool_Replication_WritesAllMembers(t *testing.T) {
	ts1, vm1 := newTestVolumeServer(t)
	client1 := volumeserver.NewClient(ts1.URL)

	// ReplicationFactor=2: local primary + 1 remote replica
	pool := newTestPool(t, 2, client1)

	data := []byte("replicated blob")
	addr, err := pool.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Verify readable via pool (uses primary)
	got, err := pool.Read(addr)
	if err != nil {
		t.Fatalf("Read via pool: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("pool read: want %q, got %q", data, got)
	}

	// Verify the replica also has the data at the same offset
	repData, err := vm1.Read(NeedleAddr{VolumeID: addr.VolumeID, Offset: addr.Offset, Size: addr.Size})
	if err != nil {
		t.Fatalf("Read from replica directly: %v", err)
	}
	if string(repData) != string(data) {
		t.Errorf("replica direct read: want %q, got %q", data, repData)
	}
}

func TestVolumePool_Replication_Failover(t *testing.T) {
	ts1, _ := newTestVolumeServer(t)
	client1 := volumeserver.NewClient(ts1.URL)

	// ReplicationFactor=2: local primary + 1 remote replica
	pool := newTestPool(t, 2, client1)

	data := []byte("failover blob")
	addr, err := pool.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Kill the local primary by closing its volumes
	pool.local.Close()

	// Read should fail over to the remote replica
	got, err := pool.Read(addr)
	if err != nil {
		t.Fatalf("Read after primary failure: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("failover read: want %q, got %q", data, got)
	}
}

func TestVolumePool_RegistryPersistence(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVM(t, 0)

	pool, err := newVolumePool(vm, nil, dir, 1, 0)
	if err != nil {
		t.Fatalf("newVolumePool: %v", err)
	}

	data := []byte("persist-me")
	addr, err := pool.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	pool2, err := newVolumePool(vm, nil, dir, 1, 0)
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
