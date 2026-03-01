// Package client provides a Go client for the brisedb TCP server.
package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
)

// Client is a connection to a brisedb server. Safe for concurrent use.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

// Dial connects to a brisedb server at addr (e.g. "localhost:6380").
func Dial(addr string) (*Client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("brisedb: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, r: bufio.NewReader(conn)}, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Set sets key to value.
func (c *Client) Set(key, value string) error {
	_, err := c.do("SET", key, value)
	return err
}

// Get returns the value for key and whether it was found.
func (c *Client) Get(key string) (string, bool, error) {
	val, err := c.do("GET", key)
	if err != nil {
		return "", false, err
	}
	if val == "nil" {
		return "", false, nil
	}
	return val, true, nil
}

// Delete removes key from the store.
func (c *Client) Delete(key string) error {
	_, err := c.do("DELETE", key)
	return err
}

// Count returns the number of keys whose value equals value.
func (c *Client) Count(value string) (int, error) {
	raw, err := c.do("COUNT", value)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscan(raw, &n); err != nil {
		return 0, fmt.Errorf("brisedb: unexpected COUNT response %q", raw)
	}
	return n, nil
}

// Begin starts a transaction.
func (c *Client) Begin() error {
	_, err := c.do("BEGIN")
	return err
}

// Commit commits the current transaction.
func (c *Client) Commit() error {
	_, err := c.do("COMMIT")
	return err
}

// Rollback discards the current transaction.
func (c *Client) Rollback() error {
	_, err := c.do("ROLLBACK")
	return err
}

// do sends a command and returns the payload of the response (stripping the +/- prefix).
// Errors from the server (- prefix) are returned as Go errors.
func (c *Client) do(parts ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	line := strings.Join(parts, " ") + "\n"
	if _, err := fmt.Fprint(c.conn, line); err != nil {
		return "", fmt.Errorf("brisedb: write: %w", err)
	}

	resp, err := c.r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("brisedb: read: %w", err)
	}
	resp = strings.TrimRight(resp, "\n")

	if strings.HasPrefix(resp, "+") {
		return resp[1:], nil
	}
	if strings.HasPrefix(resp, "-") {
		return "", fmt.Errorf("brisedb: %s", resp[1:])
	}
	return "", fmt.Errorf("brisedb: malformed response %q", resp)
}
