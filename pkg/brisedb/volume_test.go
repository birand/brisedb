package brisedb

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// helper: open a VolumeManager with a tiny rotation threshold.
func newTestVM(t *testing.T, maxSize uint64, extraDrives ...string) *VolumeManager {
	t.Helper()
	primary := filepath.Join(t.TempDir(), "volumes")
	drives := append([]string{primary}, extraDrives...)
	vm, err := newVolumeManager(drives, maxSize)
	if err != nil {
		t.Fatalf("newVolumeManager: %v", err)
	}
	t.Cleanup(func() { vm.Close() })
	return vm
}

func TestVolumeManager_WriteRead(t *testing.T) {
	vm := newTestVM(t, 0) // default size limit

	for i := 0; i < 10; i++ {
		data := []byte(fmt.Sprintf("value-%d", i))
		addr, err := vm.Write(data)
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		got, err := vm.Read(addr)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		if string(got) != string(data) {
			t.Errorf("want %q, got %q", data, got)
		}
	}
}

func TestVolumeManager_VolumeRotation(t *testing.T) {
	// Set max size so small that every write triggers a new volume.
	// Each entry is [8-byte header + payload]; payload is 5 bytes → 13 bytes total.
	// Threshold = 10 bytes forces rotation after the first write.
	vm := newTestVM(t, 10)

	addrs := make([]NeedleAddr, 5)
	for i := range addrs {
		data := []byte(fmt.Sprintf("hello"))
		addr, err := vm.Write(data)
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		addrs[i] = addr
	}

	// Each write should have gone into a different volume (IDs 0..4)
	seen := map[uint32]bool{}
	for _, a := range addrs {
		seen[a.VolumeID] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected multiple volume IDs due to rotation, got %v", seen)
	}

	// All values must still be readable
	for i, addr := range addrs {
		got, err := vm.Read(addr)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		if string(got) != "hello" {
			t.Errorf("entry %d: want hello, got %q", i, got)
		}
	}
}

func TestVolumeManager_MultipleDrives(t *testing.T) {
	drive2 := t.TempDir()
	vm := newTestVM(t, 0, drive2)

	const n = 20
	addrs := make([]NeedleAddr, n)
	for i := range addrs {
		data := []byte(fmt.Sprintf("val-%d", i))
		addr, err := vm.Write(data)
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		addrs[i] = addr
	}

	// With 2 drives, volumes should be spread across both drives
	stats := vm.Stats()
	drives := map[string]bool{}
	for _, s := range stats {
		drives[s.Drive] = true
	}
	if len(drives) < 2 {
		t.Errorf("expected writes on 2 drives, only saw: %v", drives)
	}

	// All values must be readable
	for i, addr := range addrs {
		got, err := vm.Read(addr)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		if string(got) != fmt.Sprintf("val-%d", i) {
			t.Errorf("entry %d: want val-%d, got %q", i, i, got)
		}
	}
}

func TestVolumeManager_Persistence(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "volumes")

	// Write data then close
	vm1, err := newVolumeManager([]string{primary}, 0)
	if err != nil {
		t.Fatalf("open vm1: %v", err)
	}
	addr, err := vm1.Write([]byte("persistent"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	vm1.Close()

	// Reopen and read
	vm2, err := newVolumeManager([]string{primary}, 0)
	if err != nil {
		t.Fatalf("open vm2: %v", err)
	}
	defer vm2.Close()

	got, err := vm2.Read(addr)
	if err != nil {
		t.Fatalf("Read after reopen: %v", err)
	}
	if string(got) != "persistent" {
		t.Errorf("want persistent, got %q", got)
	}
}

func TestVolumeManager_Stats(t *testing.T) {
	vm := newTestVM(t, 0)

	vm.Write([]byte("a"))
	vm.Write([]byte("bb"))

	stats := vm.Stats()
	if len(stats) == 0 {
		t.Fatal("expected at least one volume in stats")
	}
	for _, s := range stats {
		if s.Size == 0 {
			t.Errorf("volume %d has zero size in stats", s.VolumeID)
		}
		if _, err := os.Stat(s.Path); err != nil {
			t.Errorf("volume file %s not found: %v", s.Path, err)
		}
	}
}

func TestDBOptions_ExtraDrives(t *testing.T) {
	drive1 := t.TempDir()
	drive2 := t.TempDir()

	db, err := NewBriseDB(t.TempDir(), DBOptions{
		ExtraDrives:   []string{drive1, drive2},
		MaxVolumeSize: 10, // tiny threshold to force rotation
	})
	if err != nil {
		t.Fatalf("NewBriseDB: %v", err)
	}
	defer db.Close()

	s := db.NewSession()
	for i := 0; i < 30; i++ {
		if err := s.Set(fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}

	// Verify all values are readable
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("k%d", i)
		want := fmt.Sprintf("v%d", i)
		val, ok := s.Get(key)
		if !ok || val != want {
			t.Errorf("Get %s: want %q, got %q (ok=%v)", key, want, val, ok)
		}
	}

	// Verify volumes exist across multiple drives
	stats := db.VolumeStats()
	drivesSeen := map[string]bool{}
	for _, vi := range stats {
		drivesSeen[vi.Drive] = true
	}
	if len(drivesSeen) < 2 {
		t.Errorf("expected writes on ≥2 drives, got %v", drivesSeen)
	}
}

func TestDBOptions_Persistence_MultipleDrives(t *testing.T) {
	dataDir := t.TempDir()
	drive2 := t.TempDir()

	db, err := NewBriseDB(dataDir, DBOptions{ExtraDrives: []string{drive2}})
	if err != nil {
		t.Fatalf("NewBriseDB: %v", err)
	}
	s := db.NewSession()
	s.Set("foo", "bar")
	s.Set("baz", "qux")
	db.Close()

	// Reopen with same drives
	db2, err := NewBriseDB(dataDir, DBOptions{ExtraDrives: []string{drive2}})
	if err != nil {
		t.Fatalf("NewBriseDB reload: %v", err)
	}
	defer db2.Close()

	s2 := db2.NewSession()
	for _, tc := range []struct{ k, v string }{{"foo", "bar"}, {"baz", "qux"}} {
		val, ok := s2.Get(tc.k)
		if !ok || val != tc.v {
			t.Errorf("after reload: Get %s = (%q, %v), want (%q, true)", tc.k, val, ok, tc.v)
		}
	}
}
