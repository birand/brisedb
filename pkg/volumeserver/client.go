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
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
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
