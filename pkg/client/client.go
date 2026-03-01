// Package client provides a Go client for the brisedb TCP server.
package client

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
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

// SetEX sets key to value with an expiry after ttl elapses.
func (c *Client) SetEX(key, value string, ttl time.Duration) error {
	secs := int64(ttl.Seconds())
	if secs <= 0 {
		secs = 1
	}
	_, err := c.do("SET", key, value, "EX", strconv.FormatInt(secs, 10))
	return err
}

// TTL returns the remaining lifetime of key in seconds.
// Returns -1 if the key has no expiry, -2 if the key does not exist or has expired.
func (c *Client) TTL(key string) (int64, error) {
	raw, err := c.do("TTL", key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("brisedb: unexpected TTL response %q", raw)
	}
	return n, nil
}

// Persist removes the TTL from key, making it persist indefinitely.
// Returns true if the key existed and had a TTL removed.
func (c *Client) Persist(key string) (bool, error) {
	raw, err := c.do("PERSIST", key)
	if err != nil {
		return false, err
	}
	return raw == "1", nil
}

// Keys returns all keys matching pattern. Supports * and ? wildcards.
func (c *Client) Keys(pattern string) ([]string, error) {
	return c.doList("KEYS", pattern)
}

// Scan returns a paginated batch of keys starting at cursor.
// count is a hint for the batch size.
// When the returned nextCursor is 0, iteration is complete.
func (c *Client) Scan(cursor, count int) (nextCursor int, keys []string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	line := fmt.Sprintf("SCAN %d COUNT %d\n", cursor, count)
	if _, err := fmt.Fprint(c.conn, line); err != nil {
		return 0, nil, fmt.Errorf("brisedb: write: %w", err)
	}

	// First line: "+<next_cursor> <N>"
	resp, err := c.r.ReadString('\n')
	if err != nil {
		return 0, nil, fmt.Errorf("brisedb: read: %w", err)
	}
	resp = strings.TrimRight(resp, "\n")
	if strings.HasPrefix(resp, "-") {
		return 0, nil, fmt.Errorf("brisedb: %s", resp[1:])
	}
	var n int
	if _, err := fmt.Sscanf(resp[1:], "%d %d", &nextCursor, &n); err != nil {
		return 0, nil, fmt.Errorf("brisedb: malformed SCAN response %q", resp)
	}
	keys, err = c.readLines(n)
	return nextCursor, keys, err
}

// Publish sends payload to all subscribers of channel.
// Returns the number of subscribers that received the message.
func (c *Client) Publish(channel, payload string) (int, error) {
	raw, err := c.do("PUBLISH", channel, payload)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscan(raw, &n); err != nil {
		return 0, fmt.Errorf("brisedb: unexpected PUBLISH response %q", raw)
	}
	return n, nil
}

// Compact rewrites the server's WAL with only the current committed state.
func (c *Client) Compact() error {
	_, err := c.do("COMPACT")
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

// doList sends a command and reads a count-prefixed list of values.
// Server response: "+<N>\n+item1\n+item2\n..."
func (c *Client) doList(parts ...string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	line := strings.Join(parts, " ") + "\n"
	if _, err := fmt.Fprint(c.conn, line); err != nil {
		return nil, fmt.Errorf("brisedb: write: %w", err)
	}

	resp, err := c.r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("brisedb: read: %w", err)
	}
	resp = strings.TrimRight(resp, "\n")
	if strings.HasPrefix(resp, "-") {
		return nil, fmt.Errorf("brisedb: %s", resp[1:])
	}
	var n int
	if _, err := fmt.Sscan(resp[1:], &n); err != nil {
		return nil, fmt.Errorf("brisedb: malformed count %q", resp)
	}
	return c.readLines(n)
}

// readLines reads n "+value" lines. Must be called with c.mu held.
func (c *Client) readLines(n int) ([]string, error) {
	result := make([]string, 0, n)
	for i := 0; i < n; i++ {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return result, fmt.Errorf("brisedb: read: %w", err)
		}
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, "+") {
			return result, fmt.Errorf("brisedb: malformed list item %q", line)
		}
		result = append(result, line[1:])
	}
	return result, nil
}
