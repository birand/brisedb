package brisedb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/birand/brisedb/pkg/volumeserver"
)

// volumeRef identifies where a globally-assigned volume lives.
// ServerURL == "" means it lives in the local VolumeManager.
type volumeRef struct {
	ServerURL     string `json:"server_url,omitempty"`
	LocalVolumeID uint32 `json:"local_vol_id"`
}

// VolumePool abstracts blob storage across a local VolumeManager and zero or
// more remote HTTP volume servers. It assigns global volume IDs and maintains
// a registry (globalID → server + localVolumeID) persisted as JSON.
//
// Backward compatibility: when the registry is empty or a globalID is not
// found in the registry, the pool assumes the volume is local and the global
// ID equals the local volume ID (matching old WAL/index data).
type VolumePool struct {
	mu           sync.Mutex
	local        *VolumeManager
	remotes      []*volumeserver.Client // ordered list; round-robin with local
	registry     map[uint32]volumeRef
	nextID       uint32
	registryPath string // persisted at dataDir/vol-registry.json
	rrIdx        int    // next backend index (0 = local, 1..N = remote[0..N-1])
}

// newVolumePool creates a VolumePool backed by local and (optionally) remote servers.
// Existing local volume IDs are registered automatically so old data stays readable.
func newVolumePool(local *VolumeManager, remotes []*volumeserver.Client, dataDir string) (*VolumePool, error) {
	p := &VolumePool{
		local:        local,
		remotes:      remotes,
		registry:     make(map[uint32]volumeRef),
		registryPath: filepath.Join(dataDir, "vol-registry.json"),
	}

	// Load persisted registry if it exists.
	if err := p.loadRegistry(); err != nil {
		return nil, err
	}

	// Register existing local volumes that are not yet in the registry.
	for _, info := range local.Stats() {
		id := info.VolumeID
		if _, ok := p.registry[id]; !ok {
			p.registry[id] = volumeRef{LocalVolumeID: id}
			if id >= p.nextID {
				p.nextID = id + 1
			}
		}
	}

	return p, nil
}

// Write appends data to the next available backend (round-robin across local
// and all remote servers). Returns a NeedleAddr with a globally unique VolumeID.
func (p *VolumePool) Write(data []byte) (NeedleAddr, error) {
	p.mu.Lock()
	backends := len(p.remotes) + 1 // +1 for local
	idx := p.rrIdx % backends
	p.rrIdx++
	p.mu.Unlock()

	var addr NeedleAddr

	if idx == 0 {
		// Local write
		localAddr, err := p.local.Write(data)
		if err != nil {
			return NeedleAddr{}, err
		}
		globalID := p.assignGlobal(volumeRef{LocalVolumeID: localAddr.VolumeID})
		addr = NeedleAddr{VolumeID: globalID, Offset: localAddr.Offset, Size: localAddr.Size}
	} else {
		// Remote write
		client := p.remotes[idx-1]
		res, err := client.Write(data)
		if err != nil {
			return NeedleAddr{}, fmt.Errorf("remote write to %s: %w", client.BaseURL(), err)
		}
		globalID := p.assignGlobal(volumeRef{ServerURL: client.BaseURL(), LocalVolumeID: res.VolumeID})
		addr = NeedleAddr{VolumeID: globalID, Offset: res.Offset, Size: res.Size}
	}

	if err := p.saveRegistry(); err != nil {
		return NeedleAddr{}, fmt.Errorf("persist volume registry: %w", err)
	}
	return addr, nil
}

// Read retrieves a blob by its global NeedleAddr.
func (p *VolumePool) Read(addr NeedleAddr) ([]byte, error) {
	ref := p.resolveRef(addr.VolumeID)

	if ref.ServerURL == "" {
		localAddr := NeedleAddr{VolumeID: ref.LocalVolumeID, Offset: addr.Offset, Size: addr.Size}
		return p.local.Read(localAddr)
	}

	client := p.remoteByURL(ref.ServerURL)
	if client == nil {
		return nil, fmt.Errorf("volume pool: no client for server %q", ref.ServerURL)
	}
	return client.Read(ref.LocalVolumeID, addr.Offset, addr.Size)
}

// EnsureVolume ensures the local manager has volume id open (used during WAL replay).
// If the registry has no entry for id, we assume it's a legacy local volume.
func (p *VolumePool) EnsureVolume(id uint32) error {
	p.mu.Lock()
	ref, ok := p.registry[id]
	if !ok {
		// Legacy data: assume local, globalID == localVolumeID
		ref = volumeRef{LocalVolumeID: id}
		p.registry[id] = ref
		if id >= p.nextID {
			p.nextID = id + 1
		}
	}
	p.mu.Unlock()

	if ref.ServerURL == "" {
		return p.local.EnsureVolume(ref.LocalVolumeID)
	}
	return nil // remote volumes don't need local open
}

// Stats returns combined stats from local and all remote servers.
func (p *VolumePool) Stats() []VolumeInfo {
	infos := p.local.Stats()
	for _, c := range p.remotes {
		stats, err := c.Stats()
		if err != nil {
			continue // skip unreachable servers in stats
		}
		for _, s := range stats {
			infos = append(infos, VolumeInfo{
				VolumeID: s.VolumeID,
				Drive:    s.Drive,
				Path:     s.Path,
				Size:     s.Size,
			})
		}
	}
	return infos
}

// Close closes the local VolumeManager (remote clients have no persistent state).
func (p *VolumePool) Close() error {
	return p.local.Close()
}

// ------------------------------------------------------------------ //
// Helpers
// ------------------------------------------------------------------ //

// assignGlobal registers ref under a new global ID (or finds an existing one).
// If an entry with the same (ServerURL, LocalVolumeID) already exists, returns
// the existing globalID to avoid duplicates during round-robin.
func (p *VolumePool) assignGlobal(ref volumeRef) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Check if this (server, localVol) pair is already registered
	for id, r := range p.registry {
		if r.ServerURL == ref.ServerURL && r.LocalVolumeID == ref.LocalVolumeID {
			return id
		}
	}
	id := p.nextID
	p.nextID++
	p.registry[id] = ref
	return id
}

// resolveRef returns the volumeRef for globalID, defaulting to local if unknown.
func (p *VolumePool) resolveRef(globalID uint32) volumeRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ref, ok := p.registry[globalID]; ok {
		return ref
	}
	// Legacy: assume local with same ID
	return volumeRef{LocalVolumeID: globalID}
}

func (p *VolumePool) remoteByURL(url string) *volumeserver.Client {
	for _, c := range p.remotes {
		if c.BaseURL() == url {
			return c
		}
	}
	return nil
}

func (p *VolumePool) loadRegistry() error {
	f, err := os.Open(p.registryPath)
	if os.IsNotExist(err) {
		return nil // fresh start
	}
	if err != nil {
		return fmt.Errorf("open vol-registry: %w", err)
	}
	defer f.Close()

	var reg map[uint32]volumeRef
	if err := json.NewDecoder(f).Decode(&reg); err != nil {
		return fmt.Errorf("decode vol-registry: %w", err)
	}
	for id, ref := range reg {
		p.registry[id] = ref
		if id >= p.nextID {
			p.nextID = id + 1
		}
	}
	return nil
}

func (p *VolumePool) saveRegistry() error {
	p.mu.Lock()
	// Snapshot under lock
	snap := make(map[uint32]volumeRef, len(p.registry))
	for k, v := range p.registry {
		snap[k] = v
	}
	p.mu.Unlock()

	tmp := p.registryPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(snap); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, p.registryPath)
}
