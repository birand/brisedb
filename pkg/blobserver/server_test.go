package blobserver_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/birand/brisedb/pkg/blobserver"
	"github.com/birand/brisedb/pkg/brisedb"
)

func newTestServer(t *testing.T) (*httptest.Server, *brisedb.BriseDB) {
	t.Helper()
	db, err := brisedb.NewBriseDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewBriseDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := blobserver.New(db, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, db
}

func TestBlob_PutGet(t *testing.T) {
	ts, _ := newTestServer(t)

	body := []byte("hello brisedb blob")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/blobs/mykey", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/v1/blobs/mykey")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status: want 200, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("GET body: want %q, got %q", body, got)
	}
}

func TestBlob_Head(t *testing.T) {
	ts, _ := newTestServer(t)

	body := []byte("metadata test")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/blobs/headkey", bytes.NewReader(body))
	http.DefaultClient.Do(req)

	resp, err := http.Head(ts.URL + "/v1/blobs/headkey")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status: want 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Brise-Size") == "" {
		t.Error("HEAD: missing X-Brise-Size header")
	}
}

func TestBlob_Delete(t *testing.T) {
	ts, _ := newTestServer(t)

	body := []byte("delete me")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/blobs/delkey", bytes.NewReader(body))
	http.DefaultClient.Do(req)

	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/v1/blobs/delkey", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status: want 204, got %d", resp.StatusCode)
	}

	resp, _ = http.Get(ts.URL + "/v1/blobs/delkey")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("after DELETE: want 404, got %d", resp.StatusCode)
	}
}

func TestBlob_RangeRequest(t *testing.T) {
	ts, _ := newTestServer(t)

	body := []byte("0123456789abcdef")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/blobs/rangekey", bytes.NewReader(body))
	http.DefaultClient.Do(req)

	// Request bytes 4-7 (inclusive)
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/blobs/rangekey", nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET Range: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range status: want 206, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "4567" {
		t.Errorf("Range body: want %q, got %q", "4567", got)
	}
}

func TestBlob_TTL(t *testing.T) {
	ts, _ := newTestServer(t)

	body := []byte("expires soon")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/blobs/ttlkey?ttl=1h", bytes.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	resp, _ = http.Get(ts.URL + "/v1/blobs/ttlkey")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with TTL: want 200, got %d", resp.StatusCode)
	}
	ttlHdr := resp.Header.Get("X-Brise-TTL")
	if ttlHdr == "" || ttlHdr == "0" {
		t.Errorf("expected non-zero X-Brise-TTL, got %q", ttlHdr)
	}
}

func TestBlob_NotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := http.Get(ts.URL + "/v1/blobs/nosuchkey")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", resp.StatusCode)
	}
}

func TestKeys_Endpoint(t *testing.T) {
	ts, _ := newTestServer(t)

	for _, k := range []string{"foo", "bar", "baz"} {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/blobs/"+k, strings.NewReader("v"))
		http.DefaultClient.Do(req)
	}

	resp, err := http.Get(ts.URL + "/v1/keys?pattern=b*")
	if err != nil {
		t.Fatalf("GET /v1/keys: %v", err)
	}
	defer resp.Body.Close()
	var result struct {
		Keys  []string `json:"keys"`
		Count int      `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Count != 2 {
		t.Errorf("keys b*: want 2, got %d (%v)", result.Count, result.Keys)
	}
}

func TestScan_Endpoint(t *testing.T) {
	ts, _ := newTestServer(t)

	for i := 0; i < 5; i++ {
		key := "scankey" + string(rune('0'+i))
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/blobs/"+key, strings.NewReader("v"))
		http.DefaultClient.Do(req)
	}

	resp, err := http.Get(ts.URL + "/v1/scan?cursor=0&count=3")
	if err != nil {
		t.Fatalf("GET /v1/scan: %v", err)
	}
	defer resp.Body.Close()
	var result struct {
		NextCursor int      `json:"next_cursor"`
		Keys       []string `json:"keys"`
		Count      int      `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Count != 3 {
		t.Errorf("scan count: want 3, got %d", result.Count)
	}
	if result.NextCursor == 0 {
		t.Error("expected non-zero next_cursor after partial scan")
	}
}

func TestStatus_Endpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/status")
	if err != nil {
		t.Fatalf("GET /v1/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("status field: want ok, got %v", result["status"])
	}
}
