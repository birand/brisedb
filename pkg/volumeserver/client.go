package volumeserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to a remote volume server over HTTP.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient creates a Client for the volume server at baseURL (e.g. "http://host:8080").
// Each Client gets its own http.Transport so its connection pool is not shared
// with other clients or the process-wide http.DefaultTransport.
// Pool sizing is tuned for concurrent blob writes (replication fan-out).
func NewClient(baseURL string) *Client {
	t := &http.Transport{
		// Allow up to 64 idle keep-alive connections to this one server so
		// concurrent replicated writes don't queue behind each other.
		MaxIdleConnsPerHost: 64,
		MaxConnsPerHost:     64,
		// Keep connections alive for 90 s (well above a typical request RTT).
		IdleConnTimeout:     90 * time.Second,
		// Standard dial / TLS timeouts.
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true, // blobs are already binary; compression wastes CPU
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second, Transport: t},
	}
}

// WriteToVolume sends data to a specific volume on the remote server.
// Used for replicated writes to ensure all group members use the same volume ID.
func (c *Client) WriteToVolume(id uint32, data []byte) (WriteResult, error) {
	url := fmt.Sprintf("%s/write?vol=%d", c.baseURL, id)
	resp, err := c.http.Post(url, "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return WriteResult{}, fmt.Errorf("volume client write-to-volume: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return WriteResult{}, fmt.Errorf("volume client write-to-volume: server returned %d: %s", resp.StatusCode, body)
	}
	var result WriteResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return WriteResult{}, fmt.Errorf("volume client write-to-volume: decode: %w", err)
	}
	return result, nil
}

// Write sends data to the remote volume server and returns the blob address.
func (c *Client) Write(data []byte) (WriteResult, error) {
	resp, err := c.http.Post(c.baseURL+"/write", "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return WriteResult{}, fmt.Errorf("volume client write: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return WriteResult{}, fmt.Errorf("volume client write: server returned %d: %s", resp.StatusCode, body)
	}
	var result WriteResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return WriteResult{}, fmt.Errorf("volume client write: decode response: %w", err)
	}
	return result, nil
}

// Read retrieves a blob from the remote volume server.
func (c *Client) Read(volID uint32, offset, size uint64) ([]byte, error) {
	url := fmt.Sprintf("%s/read?vol=%d&off=%d&sz=%d", c.baseURL, volID, offset, size)
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("volume client read: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("volume client read: server returned %d: %s", resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

// Stats returns volume info from the remote server.
func (c *Client) Stats() ([]VolumeStat, error) {
	resp, err := c.http.Get(c.baseURL + "/status")
	if err != nil {
		return nil, fmt.Errorf("volume client status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("volume client status: server returned %d: %s", resp.StatusCode, body)
	}
	var stats []VolumeStat
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("volume client status: decode: %w", err)
	}
	return stats, nil
}

// BaseURL returns the server's base URL (used as a unique identifier).
func (c *Client) BaseURL() string { return c.baseURL }
