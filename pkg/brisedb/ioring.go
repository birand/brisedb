// Package-level io_uring abstraction for brisedb.
//
// fileReader wraps an *os.File and exposes ReadAt plus BatchReadAt.
// On Linux the concrete type is uringFile (io_uring backed).
// On all other platforms it falls back to standard pread via posixFile.
//
// Usage:
//
//	r := newFileReader(f)   // wraps *os.File
//	r.ReadAt(buf, offset)   // single read — same as f.ReadAt
//	r.BatchReadAt(reqs)     // multiple reads submitted in one syscall (Linux)
//	r.Close()
package brisedb

import "os"

// BatchReadReq is one element of a BatchReadAt call.
type BatchReadReq struct {
	Buf []byte // destination buffer (must be pre-allocated)
	Off int64  // file offset
	N   int    // bytes read (set by BatchReadAt)
	Err error  // error (set by BatchReadAt)
}

// fileReader abstracts file reads so that io_uring can be swapped in on Linux
// without changing callers.
type fileReader interface {
	// ReadAt reads len(b) bytes from the file at offset off.
	// Semantics identical to (*os.File).ReadAt.
	ReadAt(b []byte, off int64) (int, error)

	// BatchReadAt submits all requests concurrently (io_uring on Linux,
	// sequential pread otherwise) and populates req.N / req.Err for each.
	BatchReadAt(reqs []BatchReadReq)

	// Close releases any platform resources (ring fd, mmaps).
	// Does NOT close the underlying *os.File.
	Close() error
}

// posixFile is the fallback implementation: delegates directly to *os.File.
// Used on macOS, Windows, and any Linux system where io_uring setup fails.
type posixFile struct{ f *os.File }

func (p *posixFile) ReadAt(b []byte, off int64) (int, error) {
	return p.f.ReadAt(b, off)
}

func (p *posixFile) BatchReadAt(reqs []BatchReadReq) {
	for i := range reqs {
		reqs[i].N, reqs[i].Err = p.f.ReadAt(reqs[i].Buf, reqs[i].Off)
	}
}

func (p *posixFile) Close() error { return nil } // file owned by caller
