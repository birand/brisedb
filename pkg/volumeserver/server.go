// Package volumeserver implements a standalone HTTP blob storage server.
// It wraps a local VolumeManager and exposes three endpoints:
//
//	POST /write           — append a blob; returns JSON {vol_id, offset, size}
//	GET  /read            — retrieve a blob; query params: vol, off, sz
//	GET  /status          — list all volumes with their sizes
package volumeserver

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
)

// Backend is the minimal interface a VolumeServer needs from its storage layer.
type Backend interface {
	Write(data []byte) (WriteResult, error)
	// WriteToVolume appends data to the specified volume (creating it if needed).
	// Used for replicated writes where all members must use the same volume ID.
	WriteToVolume(id uint32, data []byte) (WriteResult, error)
	Read(volID uint32, offset, size uint64) ([]byte, error)
	Stats() []VolumeStat
}

// WriteResult is returned by Backend.Write.
type WriteResult struct {
	VolumeID uint32 `json:"vol_id"`
	Offset   uint64 `json:"offset"`
	Size     uint64 `json:"size"`
}

// VolumeStat describes one local volume file.
type VolumeStat struct {
	VolumeID uint32 `json:"vol_id"`
	Drive    string `json:"drive"`
	Path     string `json:"path"`
	Size     uint64 `json:"size_bytes"`
}

// Server is an HTTP volume server.
type Server struct {
	backend Backend
	mux     *http.ServeMux
	log     *slog.Logger
}

// New creates a Server backed by the given Backend.
func New(backend Backend, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{backend: backend, log: log}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/write", s.handleWrite)
	s.mux.HandleFunc("/read", s.handleRead)
	s.mux.HandleFunc("/status", s.handleStatus)
	return s
}

// Handler returns the HTTP handler for this server (useful for testing).
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe starts the HTTP server on addr (blocks).
func (s *Server) ListenAndServe(addr string) error {
	s.log.Info("volume server listening", "addr", addr)
	return http.ListenAndServe(addr, s.mux)
}

// handleWrite receives raw bytes and appends them to a volume.
// POST /write          — auto-selects volume (normal write)
// POST /write?vol=N    — appends to volume N (replica/group write)
// Body: raw blob bytes
// Response: JSON WriteResult
func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body: %v", err), http.StatusBadRequest)
		return
	}

	var result WriteResult
	if volStr := r.URL.Query().Get("vol"); volStr != "" {
		volID, err := parseUint32(volStr)
		if err != nil {
			http.Error(w, "bad vol: "+err.Error(), http.StatusBadRequest)
			return
		}
		result, err = s.backend.WriteToVolume(volID, data)
		if err != nil {
			s.log.Error("write-to-volume failed", "vol", volID, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		result, err = s.backend.Write(data)
		if err != nil {
			s.log.Error("write failed", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleRead retrieves a blob by its address.
// GET /read?vol=<id>&off=<offset>&sz=<size>
// Response: raw bytes
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	volID, err := parseUint32(q.Get("vol"))
	if err != nil {
		http.Error(w, "bad vol: "+err.Error(), http.StatusBadRequest)
		return
	}
	offset, err := parseUint64(q.Get("off"))
	if err != nil {
		http.Error(w, "bad off: "+err.Error(), http.StatusBadRequest)
		return
	}
	size, err := parseUint64(q.Get("sz"))
	if err != nil {
		http.Error(w, "bad sz: "+err.Error(), http.StatusBadRequest)
		return
	}
	data, err := s.backend.Read(volID, offset, size)
	if err != nil {
		s.log.Error("read failed", "vol", volID, "offset", offset, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

// handleStatus returns stats about all open volume files.
// GET /status
// Response: JSON array of VolumeStat
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.backend.Stats())
}

func parseUint32(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}

func parseUint64(s string) (uint64, error) {
	return strconv.ParseUint(s, 10, 64)
}
