//go:build linux

package brisedb

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// ------------------------------------------------------------------ //
// io_uring syscall numbers and constants
// ------------------------------------------------------------------ //

const (
	sysIoUringSetup = 425
	sysIoUringEnter = 426

	// Operation codes
	ioUringOpRead = 22 // IORING_OP_READ — requires kernel ≥ 5.6

	// mmap offsets for SQ ring, CQ ring, and SQE array
	ioUringOffSqRing = 0
	ioUringOffCqRing = 0x8000000
	ioUringOffSqes   = 0x10000000

	// io_uring_enter flags
	ioUringEnterGetEvents = 1
)

// ------------------------------------------------------------------ //
// io_uring kernel structures (stable ABI since Linux 5.1)
// ------------------------------------------------------------------ //

// ioUringParams is the parameter block passed to io_uring_setup (120 bytes).
type ioUringParams struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFd         uint32
	resv         [3]uint32
	sqOff        ioSqRingOffsets // 40 bytes
	cqOff        ioCqRingOffsets // 40 bytes
}

// ioSqRingOffsets describes the SQ ring layout (40 bytes).
type ioSqRingOffsets struct {
	head        uint32
	tail        uint32
	ringMask    uint32
	ringEntries uint32
	flags       uint32
	dropped     uint32
	array       uint32
	resv1       uint32
	resv2       uint64
}

// ioCqRingOffsets describes the CQ ring layout (40 bytes).
type ioCqRingOffsets struct {
	head        uint32
	tail        uint32
	ringMask    uint32
	ringEntries uint32
	overflow    uint32
	cqes        uint32
	flags       uint32
	resv1       uint32
	resv2       uint64
}

// ioUringSqe is the Submission Queue Entry (64 bytes).
type ioUringSqe struct {
	opcode      uint8
	flags       uint8
	ioprio      uint16
	fd          int32
	off         uint64
	addr        uint64
	length      uint32
	rwFlags     uint32
	userData    uint64
	bufIndex    uint16
	personality uint16
	spliceFdIn  int32
	addr3       uint64
	pad2        uint64
}

// ioUringCqe is the Completion Queue Entry (16 bytes).
type ioUringCqe struct {
	userData uint64
	res      int32
	flags    uint32
}

// ------------------------------------------------------------------ //
// uringRing — a single io_uring instance
// ------------------------------------------------------------------ //

// uringRing manages one io_uring ring.  It is safe for concurrent use from
// multiple goroutines via mu.
type uringRing struct {
	fd int

	// SQ ring — mapped from (fd, IORING_OFF_SQ_RING)
	sqMap  []byte
	sqHead *uint32 // pointer into sqMap
	sqTail *uint32
	sqMask *uint32

	// SQE array — mapped from (fd, IORING_OFF_SQES)
	sqesMap []byte
	sqes    []ioUringSqe // slice over sqesMap

	// SQ index array — inside sqMap at sqOff.array offset
	sqArray []uint32

	// CQ ring — mapped from (fd, IORING_OFF_CQ_RING)
	cqMap  []byte
	cqHead *uint32 // pointer into cqMap
	cqTail *uint32
	cqMask *uint32
	cqes   []ioUringCqe // slice over cqMap

	ringEntries uint32
	mu          sync.Mutex
}

func newUringRing(entries uint32) (*uringRing, error) {
	var params ioUringParams
	params.sqEntries = entries

	fd, _, errno := syscall.Syscall(sysIoUringSetup,
		uintptr(entries), uintptr(unsafe.Pointer(&params)), 0)
	if errno != 0 {
		return nil, errno
	}

	r := &uringRing{fd: int(fd), ringEntries: params.sqEntries}

	// Map SQ ring
	sqSize := params.sqOff.array + params.sqEntries*4
	sqMap, err := syscall.Mmap(int(fd), ioUringOffSqRing, int(sqSize),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		syscall.Close(int(fd))
		return nil, err
	}
	r.sqMap = sqMap
	r.sqHead = (*uint32)(unsafe.Pointer(&sqMap[params.sqOff.head]))
	r.sqTail = (*uint32)(unsafe.Pointer(&sqMap[params.sqOff.tail]))
	r.sqMask = (*uint32)(unsafe.Pointer(&sqMap[params.sqOff.ringMask]))
	arrBase := &sqMap[params.sqOff.array]
	r.sqArray = (*[1 << 20]uint32)(unsafe.Pointer(arrBase))[:params.sqEntries]

	// Map SQE array
	sqeSize := uintptr(params.sqEntries) * unsafe.Sizeof(ioUringSqe{})
	sqesMap, err := syscall.Mmap(int(fd), ioUringOffSqes, int(sqeSize),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		syscall.Munmap(sqMap)
		syscall.Close(int(fd))
		return nil, err
	}
	r.sqesMap = sqesMap
	r.sqes = (*[1 << 20]ioUringSqe)(unsafe.Pointer(&sqesMap[0]))[:params.sqEntries]

	// Map CQ ring
	cqSize := params.cqOff.cqes + params.cqEntries*uint32(unsafe.Sizeof(ioUringCqe{}))
	cqMap, err := syscall.Mmap(int(fd), ioUringOffCqRing, int(cqSize),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		syscall.Munmap(sqesMap)
		syscall.Munmap(sqMap)
		syscall.Close(int(fd))
		return nil, err
	}
	r.cqMap = cqMap
	r.cqHead = (*uint32)(unsafe.Pointer(&cqMap[params.cqOff.head]))
	r.cqTail = (*uint32)(unsafe.Pointer(&cqMap[params.cqOff.tail]))
	r.cqMask = (*uint32)(unsafe.Pointer(&cqMap[params.cqOff.ringMask]))
	r.cqes = (*[1 << 20]ioUringCqe)(
		unsafe.Pointer(&cqMap[params.cqOff.cqes]))[:params.cqEntries]

	return r, nil
}

func (r *uringRing) close() {
	syscall.Munmap(r.cqMap)
	syscall.Munmap(r.sqesMap)
	syscall.Munmap(r.sqMap)
	syscall.Close(r.fd)
}

// submitAndWait submits n SQEs that have been pre-filled and waits for all
// completions.  It processes up to n CQEs and fills results[i].
// Caller must hold r.mu.
func (r *uringRing) submitAndWait(n int, results []int32) error {
	_, _, errno := syscall.Syscall6(sysIoUringEnter,
		uintptr(r.fd),
		uintptr(n),        // to_submit
		uintptr(n),        // min_complete
		ioUringEnterGetEvents, // flags
		0, 0)
	if errno != 0 {
		return errno
	}
	// Drain CQEs
	cqHead := atomic.LoadUint32(r.cqHead)
	cqTail := atomic.LoadUint32(r.cqTail)
	for i := 0; cqHead != cqTail && i < n; i++ {
		cqe := &r.cqes[cqHead&*r.cqMask]
		results[cqe.userData] = cqe.res
		cqHead++
	}
	atomic.StoreUint32(r.cqHead, cqHead)
	return nil
}

// batchRead submits up to len(reqs) IORING_OP_READ operations and waits for
// all completions.  Each req is identified by its slice index used as userData.
// Caller must hold r.mu.
func (r *uringRing) batchRead(goFD int, reqs []BatchReadReq, results []int32) error {
	sqTail := atomic.LoadUint32(r.sqTail)
	mask := *r.sqMask

	for i := range reqs {
		idx := sqTail & mask
		r.sqArray[idx] = idx
		sqe := &r.sqes[idx]
		sqe.opcode = ioUringOpRead
		sqe.fd = int32(goFD)
		sqe.off = uint64(reqs[i].Off)
		sqe.addr = uint64(uintptr(unsafe.Pointer(&reqs[i].Buf[0])))
		sqe.length = uint32(len(reqs[i].Buf))
		sqe.userData = uint64(i)
		sqe.flags = 0
		sqe.ioprio = 0
		sqe.rwFlags = 0
		sqTail++
	}
	atomic.StoreUint32(r.sqTail, sqTail)

	return r.submitAndWait(len(reqs), results)
}

// ------------------------------------------------------------------ //
// uringFile — fileReader backed by a per-goroutine-serialised ring
// ------------------------------------------------------------------ //

// Global ring — created lazily, shared across all uringFile instances.
// Individual reads are serialised by globalRing.mu; this is acceptable
// because the main benefit of io_uring on NVMe is reduced syscall overhead
// and kernel-side parallelism, not user-space parallelism.
var (
	globalRing     *uringRing
	globalRingOnce sync.Once
)

func getGlobalRing() *uringRing {
	globalRingOnce.Do(func() {
		r, err := newUringRing(256)
		if err == nil {
			globalRing = r
		}
		// On failure (old kernel, container restrictions) globalRing stays nil
		// and callers fall back to pread.
	})
	return globalRing
}

// uringFile wraps an *os.File and routes reads through io_uring when
// available, falling back to pread transparently.
type uringFile struct {
	f  *os.File
	fd int // cached to avoid f.Fd() which parks the goroutine
}

func newFileReader(f *os.File) fileReader {
	r := getGlobalRing()
	if r == nil {
		return &posixFile{f}
	}
	return &uringFile{f: f, fd: int(f.Fd())}
}

// ReadAt issues a single IORING_OP_READ and waits for the completion.
func (u *uringFile) ReadAt(b []byte, off int64) (int, error) {
	r := getGlobalRing()
	if r == nil {
		return u.f.ReadAt(b, off)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	mask := *r.sqMask
	sqTail := atomic.LoadUint32(r.sqTail)
	idx := sqTail & mask

	r.sqArray[idx] = idx
	sqe := &r.sqes[idx]
	sqe.opcode = ioUringOpRead
	sqe.fd = int32(u.fd)
	sqe.off = uint64(off)
	sqe.addr = uint64(uintptr(unsafe.Pointer(&b[0])))
	sqe.length = uint32(len(b))
	sqe.userData = 0
	sqe.flags = 0
	sqe.ioprio = 0
	sqe.rwFlags = 0
	atomic.StoreUint32(r.sqTail, sqTail+1)

	var results [1]int32
	if err := r.submitAndWait(1, results[:]); err != nil {
		return 0, err
	}
	res := results[0]
	if res < 0 {
		return 0, syscall.Errno(-res)
	}
	return int(res), nil
}

// BatchReadAt submits all requests as a single io_uring batch and waits for
// all completions.  All requests must target the same file.
func (u *uringFile) BatchReadAt(reqs []BatchReadReq) {
	if len(reqs) == 0 {
		return
	}
	r := getGlobalRing()
	if r == nil {
		for i := range reqs {
			reqs[i].N, reqs[i].Err = u.f.ReadAt(reqs[i].Buf, reqs[i].Off)
		}
		return
	}

	results := make([]int32, len(reqs))
	r.mu.Lock()
	err := r.batchRead(u.fd, reqs, results)
	r.mu.Unlock()

	for i := range reqs {
		if err != nil {
			reqs[i].Err = err
			continue
		}
		if results[i] < 0 {
			reqs[i].Err = syscall.Errno(-results[i])
		} else {
			reqs[i].N = int(results[i])
		}
	}
}

func (u *uringFile) Close() error { return nil }
