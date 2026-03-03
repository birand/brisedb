package brisedb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/birand/brisedb/pkg/volumeserver"
)

// volumeGroup describes one logical write target and its replicas.
//
// When replication is enabled, all members receive every blob written to the
// group in the same order.  Because each member's volume starts empty at
// group-creation time and receives identical appends, offsets are identical
// across primary and all replicas — so a NeedleAddr read against any member
// returns the correct data.
//
// Members[0] is the primary.  Its ServerURL is the empty string when the
// primary is local.  Replicas are Members[1:].
type volumeGroup struct {
	ID      uint32         `json:"id"`
	Members []volumeMember `json:"members"` // [0] = primary
}

// volumeMember identifies one physical copy of a volume group.
// All members use the same ID for their local volume file so that
// WriteToVolume(groupID, data) always appends to the right file.
type volumeMember struct {
	ServerURL string `json:"server_url,omitempty"` // empty = local VolumeManager
}

// VolumePool distributes blob storage across a local VolumeManager and zero
// or more remote HTTP volume servers, with optional replication.
//
// # Volume groups
//
// A group is a set of N backends (1 primary + N-1 replicas) that all hold
// an identical copy of the data.  Every blob written to the group lands in
// vol-{groupID}.data on each member at the same offset.
//
// On write the pool selects a group that still has room (or creates one),
// calls WriteToVolume(groupID, data) on every member, and returns
// NeedleAddr{VolumeID: groupID, Offset: primary.Offset, Size: size}.
//
// On read the pool tries members in order and returns the first success,
// providing automatic failover when a member is down.
type VolumePool struct {
	mu                sync.Mutex
	local             *VolumeManager
	remotes           []*volumeserver.Client
	registry          map[uint32]*volumeGroup
	nextID            uint32
	activeGroup       *volumeGroup // current write target (nil = none yet)
	replicationFactor int          // total copies per group (1 = no replication)
	registryPath      string
	cache             *lruCache // nil = disabled
}

// newVolumePool creates a VolumePool.
// replicationFactor controls how many copies each blob gets (default 1).
// cacheBytes sets the in-memory LRU read cache size (0 = disabled).
// Existing local volumes are registered for backward compatibility.
func newVolumePool(local *VolumeManager, remotes []*volumeserver.Client, dataDir string, replicationFactor int, cacheBytes uint64) (*VolumePool, error) {
	if replicationFactor < 1 {
		replicationFactor = 1
	}
	p := &VolumePool{
		local:             local,
		remotes:           remotes,
		registry:          make(map[uint32]*volumeGroup),
		replicationFactor: replicationFactor,
		registryPath:      filepath.Join(dataDir, "vol-registry.json"),
		cache:             newLRUCache(cacheBytes),
	}

	if err := p.loadRegistry(); err != nil {
		return nil, err
	}

	// Register existing local volumes as single-member groups for compat.
	for _, info := range local.Stats() {
		id := info.VolumeID
		if _, ok := p.registry[id]; !ok {
			p.registry[id] = &volumeGroup{
				ID:      id,
				Members: []volumeMember{{}}, // empty ServerURL = local
			}
			if id >= p.nextID {
				p.nextID = id + 1
			}
		}
	}

	return p, nil
}

// Write appends data to the active volume group, replicating to all members.
// Returns a NeedleAddr whose Offset is the primary member's offset (identical
// on all members due to group-based writes).
func (p *VolumePool) Write(data []byte) (NeedleAddr, error) {
	p.mu.Lock()
	group, err := p.getOrCreateGroup()
	p.mu.Unlock()
	if err != nil {
		return NeedleAddr{}, err
	}

	// Write to primary first to get the canonical offset.
	primaryAddr, err := p.writeToMember(group.Members[0], group.ID, data)
	if err != nil {
		return NeedleAddr{}, fmt.Errorf("volume group %d primary write: %w", group.ID, err)
	}

	// Replicate to remaining members concurrently.
	if len(group.Members) > 1 {
		type replicaErr struct{ idx int; err error }
		errCh := make(chan replicaErr, len(group.Members)-1)
		for i := 1; i < len(group.Members); i++ {
			i := i
			go func() {
				_, err := p.writeToMember(group.Members[i], group.ID, data)
				errCh <- replicaErr{i, err}
			}()
		}
		for range group.Members[1:] {
			if re := <-errCh; re.err != nil {
				return NeedleAddr{}, fmt.Errorf("volume group %d replica %d write: %w",
					group.ID, re.idx, re.err)
			}
		}
	}

	return NeedleAddr{VolumeID: group.ID, Offset: primaryAddr.Offset, Size: primaryAddr.Size}, nil
}

// Read retrieves a blob, checking the LRU cache first.
// On a cache miss it tries group members in order (automatic failover)
// and populates the cache on success.
func (p *VolumePool) Read(addr NeedleAddr) ([]byte, error) {
	// Cache lookup — no lock needed, lruCache is internally synchronized.
	if p.cache != nil {
		if data, ok := p.cache.get(addr); ok {
			return data, nil
		}
	}

	p.mu.Lock()
	group := p.registry[addr.VolumeID]
	p.mu.Unlock()

	var (
		data    []byte
		lastErr error
	)

	if group == nil {
		// Legacy data: assume single local member.
		data, lastErr = p.local.Read(addr)
	} else {
		for _, m := range group.Members {
			data, lastErr = p.readFromMember(m, group.ID, addr.Offset, addr.Size)
			if lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			return nil, fmt.Errorf("volume group %d: all members failed (last: %w)", addr.VolumeID, lastErr)
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}

	// Populate cache for future reads.
	if p.cache != nil {
		p.cache.set(addr, data)
	}
	return data, nil
}

// CacheStats returns LRU cache metrics. Returns zero-value stats if the
// cache is disabled.
func (p *VolumePool) CacheStats() LRUStats {
	if p.cache == nil {
		return LRUStats{}
	}
	return p.cache.Stats()
}

// EnsureVolume opens a local volume by ID (used during WAL replay).
func (p *VolumePool) EnsureVolume(id uint32) error {
	p.mu.Lock()
	if _, ok := p.registry[id]; !ok {
		p.registry[id] = &volumeGroup{
			ID:      id,
			Members: []volumeMember{{}},
		}
		if id >= p.nextID {
			p.nextID = id + 1
		}
	}
	p.mu.Unlock()
	return p.local.EnsureVolume(id)
}

// Stats returns combined stats from local and all remote servers.
func (p *VolumePool) Stats() []VolumeInfo {
	infos := p.local.Stats()
	seen := make(map[string]bool)
	for _, c := range p.remotes {
		if seen[c.BaseURL()] {
			continue
		}
		seen[c.BaseURL()] = true
		stats, err := c.Stats()
		if err != nil {
			continue
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

// Close closes the local VolumeManager.
func (p *VolumePool) Close() error {
	if err := p.saveRegistry(); err != nil {
		_ = p.local.Close()
		return err
	}
	return p.local.Close()
}

// ------------------------------------------------------------------ //
// Internal helpers
// ------------------------------------------------------------------ //

// getOrCreateGroup returns the active group, creating a new one if needed.
// Caller must hold p.mu.
func (p *VolumePool) getOrCreateGroup() (*volumeGroup, error) {
	// Reuse active group if it hasn't been rotated away by the local manager.
	if p.activeGroup != nil {
		primary := p.activeGroup.Members[0]
		// Check if the local manager still has room (i.e. the active local volume
		// matches the group ID). If the local manager rotated, the next call to
		// WriteToVolume will create a fresh file anyway — detect this lazily.
		if primary.ServerURL == "" {
			// Still valid as long as local hasn't silently rotated.
			// We detect rotation by checking nextID on the local manager.
			p.local.mu.Lock()
			localCurrent := p.local.driveCurrent[0]
			p.local.mu.Unlock()
			if localCurrent != nil && localCurrent.id == p.activeGroup.ID {
				return p.activeGroup, nil
			}
		} else {
			return p.activeGroup, nil
		}
	}

	// Create a new group.
	groupID := p.nextID
	p.nextID++

	backends := p.allBackends() // local first, then remotes
	n := p.replicationFactor
	if n > len(backends) {
		n = len(backends)
	}

	members := make([]volumeMember, n)
	for i := 0; i < n; i++ {
		members[i] = backends[i%len(backends)]
	}

	group := &volumeGroup{ID: groupID, Members: members}
	p.registry[groupID] = group
	p.activeGroup = group

	if err := p.saveRegistryLocked(); err != nil {
		return nil, err
	}
	return group, nil
}

// allBackends returns all backends (local first, then remotes) as volumeMembers.
func (p *VolumePool) allBackends() []volumeMember {
	out := make([]volumeMember, 0, 1+len(p.remotes))
	out = append(out, volumeMember{}) // local
	for _, c := range p.remotes {
		out = append(out, volumeMember{ServerURL: c.BaseURL()})
	}
	return out
}

func (p *VolumePool) writeToMember(m volumeMember, groupID uint32, data []byte) (NeedleAddr, error) {
	if m.ServerURL == "" {
		return p.local.WriteToVolume(groupID, data)
	}
	client := p.remoteByURL(m.ServerURL)
	if client == nil {
		return NeedleAddr{}, fmt.Errorf("no client for %q", m.ServerURL)
	}
	res, err := client.WriteToVolume(groupID, data)
	if err != nil {
		return NeedleAddr{}, err
	}
	return NeedleAddr{VolumeID: res.VolumeID, Offset: res.Offset, Size: res.Size}, nil
}

func (p *VolumePool) readFromMember(m volumeMember, localVolID uint32, offset, size uint64) ([]byte, error) {
	if m.ServerURL == "" {
		return p.local.Read(NeedleAddr{VolumeID: localVolID, Offset: offset, Size: size})
	}
	client := p.remoteByURL(m.ServerURL)
	if client == nil {
		return nil, fmt.Errorf("no client for %q", m.ServerURL)
	}
	return client.Read(localVolID, offset, size)
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
		return nil
	}
	if err != nil {
		return fmt.Errorf("open vol-registry: %w", err)
	}
	defer f.Close()

	var reg map[uint32]*volumeGroup
	if err := json.NewDecoder(f).Decode(&reg); err != nil {
		// Registry might be old format (Phase 4); silently start fresh.
		return nil
	}
	for id, g := range reg {
		g.ID = id
		p.registry[id] = g
		if id >= p.nextID {
			p.nextID = id + 1
		}
	}
	return nil
}

// saveRegistry persists the current registry to disk.
// Must NOT be called with p.mu held; use saveRegistryLocked when holding the lock.
func (p *VolumePool) saveRegistry() error {
	p.mu.Lock()
	snap := make(map[uint32]*volumeGroup, len(p.registry))
	for k, v := range p.registry {
		snap[k] = v
	}
	p.mu.Unlock()
	return p.writeRegistrySnap(snap)
}

// saveRegistryLocked persists the registry snapshot already held under p.mu.
// Caller must hold p.mu when calling.
func (p *VolumePool) saveRegistryLocked() error {
	snap := make(map[uint32]*volumeGroup, len(p.registry))
	for k, v := range p.registry {
		snap[k] = v
	}
	return p.writeRegistrySnap(snap)
}

func (p *VolumePool) writeRegistrySnap(snap map[uint32]*volumeGroup) error {
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
