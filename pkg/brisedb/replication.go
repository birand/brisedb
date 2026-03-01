package brisedb

import "sync"

// replicationManager fans WAL entries out to connected replicas.
// Each replica has a buffered channel; if the buffer is full the entry
// is dropped and the replica will be disconnected on the next write failure.
type replicationManager struct {
	mu       sync.Mutex
	replicas map[chan []byte]struct{}
}

func newReplicationManager() *replicationManager {
	return &replicationManager{replicas: make(map[chan []byte]struct{})}
}

func (rm *replicationManager) Register(ch chan []byte) {
	rm.mu.Lock()
	rm.replicas[ch] = struct{}{}
	rm.mu.Unlock()
}

func (rm *replicationManager) Unregister(ch chan []byte) {
	rm.mu.Lock()
	delete(rm.replicas, ch)
	rm.mu.Unlock()
}

func (rm *replicationManager) fanout(data []byte) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for ch := range rm.replicas {
		// Make a copy so each replica gets its own slice.
		buf := make([]byte, len(data))
		copy(buf, data)
		select {
		case ch <- buf:
		default:
			// Replica too slow — entry dropped; replica will desync and
			// will be removed when the next write to its conn fails.
		}
	}
}

func (rm *replicationManager) count() int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return len(rm.replicas)
}
