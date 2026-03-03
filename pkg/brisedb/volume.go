package brisedb

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// DefaultMaxVolumeSize is 2 GiB — a new volume file is created in the same
// drive once the active one reaches this size.
const DefaultMaxVolumeSize uint64 = 2 * 1024 * 1024 * 1024

// NeedleAddr locates a value blob within a volume file.
type NeedleAddr struct {
	VolumeID uint32
	Offset   uint64
	Size     uint64
}

// VolumeInfo describes one volume file for stats / inspection.
type VolumeInfo struct {
	VolumeID uint32
	Drive    string
	Path     string
	Size     uint64 // current byte count (including headers)
}

// volume is an append-only binary file that stores raw value blobs.
//
// On-disk format per entry:
//
//	[8-byte big-endian payload size][payload bytes]
//
// The caller stores (VolumeID, Offset, Size) in the WAL index.
// On read: seek to Offset, verify size header, return payload.
type volume struct {
	id    uint32
	drive string // parent drive directory (for stats)
	path  string
	mu    sync.Mutex
	f     *os.File
	end   uint64 // next-write byte offset
}

func openVolume(id uint32, drive, path string) (*volume, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &volume{id: id, drive: drive, path: path, f: f, end: uint64(info.Size())}, nil
}

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

func (v *volume) size() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.end
}

func (v *volume) close() error { return v.f.Close() }

// ------------------------------------------------------------------ //
// VolumeManager
// ------------------------------------------------------------------ //

// VolumeManager manages volume files spread across one or more drive
// directories. Writes are distributed across drives in round-robin order.
// When the active volume for a drive reaches maxVolumeSize, a new volume
// file is created in that drive automatically.
type VolumeManager struct {
	mu            sync.Mutex
	drives        []string          // ordered list of drive directories
	volumes       map[uint32]*volume // all known volumes, keyed by ID
	driveCurrent  []*volume         // active write volume per drive
	driveIdx      int               // next drive index for round-robin
	nextID        uint32
	maxVolumeSize uint64
}

// NewVolumeManager opens (or creates) volumes in each drive directory.
// drives must contain at least one entry. Exported for use by external
// tools such as the standalone volume server binary.
func NewVolumeManager(drives []string, maxVolumeSize uint64) (*VolumeManager, error) {
	return newVolumeManager(drives, maxVolumeSize)
}

// newVolumeManager is the internal constructor.
func newVolumeManager(drives []string, maxVolumeSize uint64) (*VolumeManager, error) {
	if maxVolumeSize == 0 {
		maxVolumeSize = DefaultMaxVolumeSize
	}

	vm := &VolumeManager{
		drives:        drives,
		volumes:       make(map[uint32]*volume),
		driveCurrent:  make([]*volume, len(drives)),
		maxVolumeSize: maxVolumeSize,
	}

	// Scan each drive for existing volume files and open them.
	for di, dir := range drives {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create drive dir %s: %w", dir, err)
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read drive dir %s: %w", dir, err)
		}

		// Collect and sort volume files so we open them in ID order.
		var volFiles []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "vol-") && strings.HasSuffix(e.Name(), ".data") {
				volFiles = append(volFiles, e.Name())
			}
		}
		sort.Strings(volFiles)

		var lastVol *volume
		for _, name := range volFiles {
			var id uint32
			if _, err := fmt.Sscanf(name, "vol-%d.data", &id); err != nil {
				continue
			}
			path := filepath.Join(dir, name)
			v, err := openVolume(id, dir, path)
			if err != nil {
				return nil, fmt.Errorf("open volume %s: %w", path, err)
			}
			vm.volumes[id] = v
			if id >= vm.nextID {
				vm.nextID = id + 1
			}
			lastVol = v
		}
		vm.driveCurrent[di] = lastVol // may be nil if drive is empty
	}

	// Ensure every drive has at least one active volume.
	for di, dir := range drives {
		if vm.driveCurrent[di] == nil {
			v, err := vm.createVolume(di, dir)
			if err != nil {
				return nil, err
			}
			vm.driveCurrent[di] = v
		}
	}

	return vm, nil
}

// createVolume allocates a new volume file in dir. Caller must hold vm.mu
// OR be in single-threaded init.
func (vm *VolumeManager) createVolume(driveIdx int, dir string) (*volume, error) {
	id := vm.nextID
	vm.nextID++
	path := filepath.Join(dir, fmt.Sprintf("vol-%06d.data", id))
	v, err := openVolume(id, dir, path)
	if err != nil {
		return nil, fmt.Errorf("create volume %s: %w", path, err)
	}
	vm.volumes[id] = v
	vm.driveCurrent[driveIdx] = v
	return v, nil
}

// EnsureVolume opens volume id if not already loaded (used during WAL replay).
// It searches all known drives for the file.
func (vm *VolumeManager) EnsureVolume(id uint32) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if _, ok := vm.volumes[id]; ok {
		return nil
	}
	for _, dir := range vm.drives {
		path := filepath.Join(dir, fmt.Sprintf("vol-%06d.data", id))
		if _, err := os.Stat(path); err == nil {
			v, err := openVolume(id, dir, path)
			if err != nil {
				return err
			}
			vm.volumes[id] = v
			if id >= vm.nextID {
				vm.nextID = id + 1
			}
			return nil
		}
	}
	return fmt.Errorf("volume %d not found in any drive", id)
}

// WriteToVolume appends data to a specific volume identified by id.
// The volume is opened (or created in the first drive) if not already loaded.
// This is used by VolumePool to write to a group-assigned volume ID so that
// all replica members maintain identical byte sequences and offsets.
func (vm *VolumeManager) WriteToVolume(id uint32, data []byte) (NeedleAddr, error) {
	vm.mu.Lock()
	v, ok := vm.volumes[id]
	if !ok {
		// Create the volume in the primary (first) drive.
		dir := vm.drives[0]
		path := filepath.Join(dir, fmt.Sprintf("vol-%06d.data", id))
		newV, err := openVolume(id, dir, path)
		if err != nil {
			vm.mu.Unlock()
			return NeedleAddr{}, fmt.Errorf("WriteToVolume create %d: %w", id, err)
		}
		vm.volumes[id] = newV
		if id >= vm.nextID {
			vm.nextID = id + 1
		}
		v = newV
	}
	vm.mu.Unlock()
	return v.write(data)
}

// Write appends data to the next available drive's active volume,
// rotating to a new volume file if the current one is full.
func (vm *VolumeManager) Write(data []byte) (NeedleAddr, error) {
	vm.mu.Lock()

	// Round-robin drive selection
	di := vm.driveIdx % len(vm.drives)
	vm.driveIdx++

	v := vm.driveCurrent[di]

	// Rotate to a new volume if current one has reached its size limit
	if v.size() >= vm.maxVolumeSize {
		newV, err := vm.createVolume(di, vm.drives[di])
		if err != nil {
			vm.mu.Unlock()
			return NeedleAddr{}, err
		}
		v = newV
	}

	vm.mu.Unlock()
	return v.write(data)
}

// Read retrieves a blob by its needle address.
func (vm *VolumeManager) Read(addr NeedleAddr) ([]byte, error) {
	vm.mu.Lock()
	v, ok := vm.volumes[addr.VolumeID]
	vm.mu.Unlock()

	if !ok {
		// Try to open it (handles volumes created by other processes / after restart)
		if err := vm.EnsureVolume(addr.VolumeID); err != nil {
			return nil, err
		}
		vm.mu.Lock()
		v = vm.volumes[addr.VolumeID]
		vm.mu.Unlock()
	}
	return v.read(addr)
}

// Stats returns a snapshot of all open volumes for monitoring.
func (vm *VolumeManager) Stats() []VolumeInfo {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	infos := make([]VolumeInfo, 0, len(vm.volumes))
	for id, v := range vm.volumes {
		infos = append(infos, VolumeInfo{
			VolumeID: id,
			Drive:    v.drive,
			Path:     v.path,
			Size:     v.size(),
		})
	}
	// Sort by ID for deterministic output
	sort.Slice(infos, func(i, j int) bool { return infos[i].VolumeID < infos[j].VolumeID })
	return infos
}

// NumVolumes returns the total number of open volume files.
func (vm *VolumeManager) NumVolumes() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return len(vm.volumes)
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
