package volumeserver_test

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/birand/brisedb/pkg/volumeserver"
)

// memBackend is an in-memory Backend for unit tests.
type memBackend struct {
	blobs [][]byte
}

func (m *memBackend) WriteToVolume(_ uint32, data []byte) (volumeserver.WriteResult, error) {
	return m.Write(data) // reuse same logic for tests
}

func (m *memBackend) Write(data []byte) (volumeserver.WriteResult, error) {
	id := uint32(len(m.blobs))
	m.blobs = append(m.blobs, data)
	return volumeserver.WriteResult{VolumeID: 0, Offset: uint64(id), Size: uint64(len(data))}, nil
}

func (m *memBackend) Read(volID uint32, offset, size uint64) ([]byte, error) {
	idx := int(offset)
	if idx >= len(m.blobs) {
		return nil, nil
	}
	return m.blobs[idx], nil
}

func (m *memBackend) Stats() []volumeserver.VolumeStat {
	return []volumeserver.VolumeStat{{VolumeID: 0, Drive: "/tmp", Path: "/tmp/vol-0.data", Size: 100}}
}

func TestServer_WriteRead(t *testing.T) {
	srv := volumeserver.New(&memBackend{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := volumeserver.NewClient(ts.URL)

	payload := []byte("hello brisedb")
	res, err := client.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Size != uint64(len(payload)) {
		t.Errorf("Size: want %d, got %d", len(payload), res.Size)
	}

	got, err := client.Read(res.VolumeID, res.Offset, res.Size)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Read: want %q, got %q", payload, got)
	}
}

func TestServer_Status(t *testing.T) {
	srv := volumeserver.New(&memBackend{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := volumeserver.NewClient(ts.URL)
	stats, err := client.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats) == 0 {
		t.Error("expected at least one stat entry")
	}
}
