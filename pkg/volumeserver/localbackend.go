package volumeserver

// FuncBackend implements Backend using injected functions.
// The cmd/volume-server binary uses this to wire a brisedb.VolumeManager
// into the HTTP server without creating a circular import.
type FuncBackend struct {
	WriteFn func(data []byte) (WriteResult, error)
	ReadFn  func(volID uint32, offset, size uint64) ([]byte, error)
	StatsFn func() []VolumeStat
}

func (f *FuncBackend) Write(data []byte) (WriteResult, error) { return f.WriteFn(data) }
func (f *FuncBackend) Read(volID uint32, offset, size uint64) ([]byte, error) {
	return f.ReadFn(volID, offset, size)
}
func (f *FuncBackend) Stats() []VolumeStat { return f.StatsFn() }
