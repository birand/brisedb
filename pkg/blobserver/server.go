// Package blobserver exposes brisedb as an HTTP blob store.
//
// Endpoints:
//
//	PUT    /v1/blobs/{key}             — store blob (body = raw bytes); ?ttl=30s optional
//	GET    /v1/blobs/{key}             — retrieve blob (supports Range header)
//	HEAD   /v1/blobs/{key}             — metadata only (no body)
//	DELETE /v1/blobs/{key}             — delete blob
//	GET    /v1/keys?pattern=*          — list keys matching glob pattern
//	GET    /v1/scan?cursor=0&count=100 — paginated key scan
//	GET    /v1/status                  — server health + volume stats
package blobserver

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/birand/brisedb/pkg/brisedb"
)

// Server is an HTTP blob gateway backed by a BriseDB instance.
type Server struct {
	db  *brisedb.BriseDB
	mux *http.ServeMux
	log *slog.Logger
}

// New creates a Server wrapping db.
func New(db *brisedb.BriseDB, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{db: db, log: log}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/v1/blobs/", s.handleBlob)
	s.mux.HandleFunc("/v1/keys", s.handleKeys)
	s.mux.HandleFunc("/v1/scan", s.handleScan)
	s.mux.HandleFunc("/v1/status", s.handleStatus)
	return s
}

// Handler returns the HTTP handler (useful for httptest).
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe starts the HTTP server on addr.
func (s *Server) ListenAndServe(addr string) error {
	s.log.Info("blob server listening", "addr", addr)
	return http.ListenAndServe(addr, s.mux)
}

// ------------------------------------------------------------------ //
// PUT/GET/HEAD/DELETE /v1/blobs/{key}
// ------------------------------------------------------------------ //

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/v1/blobs/")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		s.putBlob(w, r, key)
	case http.MethodGet:
		s.getBlob(w, r, key, true)
	case http.MethodHead:
		s.getBlob(w, r, key, false)
	case http.MethodDelete:
		s.deleteBlob(w, r, key)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// putBlob handles PUT /v1/blobs/{key}
// Query params: ttl=<duration>  (e.g. "30s", "5m", "2h")
// Body: raw blob bytes (streamed directly to volume storage)
// Response: JSON {"key":"...","size":N,"expires_at":T}
func (s *Server) putBlob(w http.ResponseWriter, r *http.Request, key string) {
	var ttl time.Duration
	if raw := r.URL.Query().Get("ttl"); raw != "" {
		var err error
		ttl, err = time.ParseDuration(raw)
		if err != nil {
			http.Error(w, "bad ttl: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body: %v", err), http.StatusBadRequest)
		return
	}

	sess := s.db.NewSession()
	if err := sess.SetBlob(key, data, ttl); err != nil {
		s.log.Error("putBlob failed", "key", key, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var expiresAt int64
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl).UnixNano()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"key":        key,
		"size":       len(data),
		"expires_at": expiresAt,
	})
}

// getBlob handles GET and HEAD /v1/blobs/{key}
// Supports HTTP Range header for partial reads (e.g. for resumable downloads).
// Response headers:
//
//	Content-Type: application/octet-stream
//	Content-Length: <size>
//	X-Brise-TTL: <remaining seconds> (or -1 if no expiry)
//	X-Brise-Size: <full blob size>
//	Accept-Ranges: bytes
func (s *Server) getBlob(w http.ResponseWriter, r *http.Request, key string, sendBody bool) {
	sess := s.db.NewSession()
	meta, ok := sess.GetBlobMeta(key)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	size := int64(meta.Addr.Size)

	// TTL header
	var ttlSecs int64 = -1
	if meta.ExpiresAt > 0 {
		rem := time.Until(time.Unix(0, meta.ExpiresAt))
		ttlSecs = int64(rem.Seconds())
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Brise-Size", strconv.FormatInt(size, 10))
	w.Header().Set("X-Brise-TTL", strconv.FormatInt(ttlSecs, 10))

	if !sendBody {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	// Parse Range header
	rangeHdr := r.Header.Get("Range")
	if rangeHdr != "" {
		start, length, err := parseRange(rangeHdr, size)
		if err != nil {
			http.Error(w, "bad range: "+err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		chunk, err := sess.ReadBlobAt(meta.Addr, start, length)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+int64(len(chunk))-1, size))
		w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(chunk)
		return
	}

	// Full blob
	data, err := sess.ReadBlobAt(meta.Addr, 0, -1)
	if err != nil {
		s.log.Error("getBlob failed", "key", key, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) deleteBlob(w http.ResponseWriter, r *http.Request, key string) {
	sess := s.db.NewSession()
	if err := sess.Delete(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ //
// GET /v1/keys?pattern=*
// ------------------------------------------------------------------ //

// handleKeys returns all keys matching a glob pattern.
// Response: JSON {"keys":["k1","k2",...],"count":N}
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		pattern = "*"
	}
	sess := s.db.NewSession()
	keys, err := sess.Keys(pattern)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"keys": keys, "count": len(keys)})
}

// ------------------------------------------------------------------ //
// GET /v1/scan?cursor=0&count=100
// ------------------------------------------------------------------ //

// handleScan returns a paginated slice of keys.
// Response: JSON {"next_cursor":N,"keys":["k1",...],"count":N}
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cursor, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	if count <= 0 {
		count = 100
	}
	sess := s.db.NewSession()
	next, keys := sess.Scan(cursor, count)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"next_cursor": next,
		"keys":        keys,
		"count":       len(keys),
	})
}

// ------------------------------------------------------------------ //
// GET /v1/status
// ------------------------------------------------------------------ //

// handleStatus returns server health and volume stats.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stats := s.db.VolumeStats()
	type volStat struct {
		VolumeID uint32 `json:"vol_id"`
		Drive    string `json:"drive"`
		Path     string `json:"path"`
		Size     uint64 `json:"size_bytes"`
	}
	vs := make([]volStat, len(stats))
	for i, v := range stats {
		vs[i] = volStat{v.VolumeID, v.Drive, v.Path, v.Size}
	}
	cs := s.db.CacheStats()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"volumes": vs,
		"cache": map[string]any{
			"entries":    cs.Entries,
			"bytes_used": cs.BytesUsed,
			"bytes_max":  cs.BytesMax,
			"hits":       cs.Hits,
			"misses":     cs.Misses,
			"hit_rate":   cs.HitRate(),
			"evictions":  cs.Evictions,
		},
	})
}

// ------------------------------------------------------------------ //
// Range header parsing
// ------------------------------------------------------------------ //

// parseRange parses a single "bytes=start-end" range header.
// Returns (start, length) clamped to [0, total).
// length == -1 means read to end.
func parseRange(hdr string, total int64) (start, length int64, err error) {
	if !strings.HasPrefix(hdr, "bytes=") {
		return 0, 0, fmt.Errorf("unsupported range unit")
	}
	spec := strings.TrimPrefix(hdr, "bytes=")
	// Only handle single range (no multi-range)
	if strings.Contains(spec, ",") {
		return 0, 0, fmt.Errorf("multi-range not supported")
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range spec")
	}
	startStr, endStr := parts[0], parts[1]

	if startStr == "" {
		// suffix range: bytes=-N  → last N bytes
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, fmt.Errorf("invalid suffix range")
		}
		start = total - n
		if start < 0 {
			start = 0
		}
		return start, total - start, nil
	}

	start, err = strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("invalid range start")
	}
	if start >= total {
		return 0, 0, fmt.Errorf("range start beyond content length")
	}
	if endStr == "" {
		return start, -1, nil // read to end
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, fmt.Errorf("invalid range end")
	}
	if end >= total {
		end = total - 1
	}
	return start, end - start + 1, nil
}
