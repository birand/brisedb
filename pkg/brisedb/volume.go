package brisedb

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// NeedleAddr locates a value blob within a volume file.
type NeedleAddr struct {
	VolumeID uint32
	Offset   uint64
	Size     uint64
}

// volume is an append-only binary file that stores raw value blobs.
//
// On-disk format per entry:
//
//	[8-byte big-endian size][size bytes of data]
//
// The caller tracks (offset, size) in the index; the size prefix lets us
// detect corruption (size mismatch between index and file).
type volume struct {
	id  uint32
	mu  sync.Mutex
	f   *os.File
	end uint64 // byte offset of the next append
}

func openVolume(id uint32, path string) (*volume, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &volume{id: id, f: f, end: uint64(info.Size())}, nil
}

// write appends data and returns the needle address.
func (v *volume) write(data []byte) (NeedleAddr, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	offset := v.end
	var hdr [8]byte
	binary.BigEndian.PutUint64(hdr[:], uint64(len(data)))
	if _, err := v.f.Write(hdr[:]); err != nil {
		return NeedleAddr{}, fmt.Errorf("volume %d: write header: %w", v.id, err)
	}
	if _, err := v.f.Write(data); err != nil {
		return NeedleAddr{}, fmt.Errorf("volume %d: write data: %w", v.id, err)
	}
	v.end += 8 + uint64(len(data))
	return NeedleAddr{VolumeID: v.id, Offset: offset, Size: uint64(len(data))}, nil
}

// read returns the blob stored at addr.
func (v *volume) read(addr NeedleAddr) ([]byte, error) {
	var hdr [8]byte
	if _, err := v.f.ReadAt(hdr[:], int64(addr.Offset)); err != nil {
		return nil, fmt.Errorf("volume %d: read header at %d: %w", v.id, addr.Offset, err)
	}
	size := binary.BigEndian.Uint64(hdr[:])
	if size != addr.Size {
		return nil, fmt.Errorf("volume %d: size mismatch at %d: index=%d file=%d",
			v.id, addr.Offset, addr.Size, size)
	}
	buf := make([]byte, size)
	if _, err := v.f.ReadAt(buf, int64(addr.Offset)+8); err != nil {
		return nil, fmt.Errorf("volume %d: read data at %d: %w", v.id, addr.Offset, err)
	}
	return buf, nil
}

func (v *volume) close() error { return v.f.Close() }

// ------------------------------------------------------------------ //
// VolumeManager
// ------------------------------------------------------------------ //

// VolumeManager manages a set of volume files inside a directory.
type VolumeManager struct {
	mu      sync.Mutex
	dir     string
	volumes map[uint32]*volume
	current *volume
	nextID  uint32
}

func newVolumeManager(dir string) (*VolumeManager, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create volume dir %s: %w", dir, err)
	}
	vm := &VolumeManager{
		dir:     dir,
		volumes: make(map[uint32]*volume),
	}
	v, err := openVolume(0, filepath.Join(dir, "vol-000000.data"))
	if err != nil {
		return nil, fmt.Errorf("open initial volume: %w", err)
	}
	vm.volumes[0] = v
	vm.current = v
	vm.nextID = 1
	return vm, nil
}

// ensureVolume opens volume id if not already open. Caller must hold vm.mu.
func (vm *VolumeManager) ensureVolume(id uint32) (*volume, error) {
	if v, ok := vm.volumes[id]; ok {
		return v, nil
	}
	path := filepath.Join(vm.dir, fmt.Sprintf("vol-%06d.data", id))
	v, err := openVolume(id, path)
	if err != nil {
		return nil, err
	}
	vm.volumes[id] = v
	if id >= vm.nextID {
		vm.nextID = id + 1
	}
	return v, nil
}

// EnsureVolume opens volume id for reading (called during WAL replay).
func (vm *VolumeManager) EnsureVolume(id uint32) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	_, err := vm.ensureVolume(id)
	return err
}

// Write appends data to the current volume.
func (vm *VolumeManager) Write(data []byte) (NeedleAddr, error) {
	vm.mu.Lock()
	v := vm.current
	vm.mu.Unlock()
	return v.write(data)
}

// Read retrieves a blob by its needle address.
func (vm *VolumeManager) Read(addr NeedleAddr) ([]byte, error) {
	vm.mu.Lock()
	v, err := vm.ensureVolume(addr.VolumeID)
	vm.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return v.read(addr)
}

// Close closes all open volume files.
func (vm *VolumeManager) Close() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	var last error
	for _, v := range vm.volumes {
		if err := v.close(); err != nil {
			last = err
		}
	}
	return last
}
