package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
)

// ReplicaConn connects to a primary brisedb server and streams WAL entries.
// Callers provide an ApplyFunc that is invoked for each JSON entry line.
type ReplicaConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// DialReplica connects to primaryAddr and sends the REPLICATE handshake.
// It reads (and discards) the snapshot entries, waiting for +READY, then
// returns a ReplicaConn ready to stream live entries.
//
// applyFn is called for each snapshot entry (before +READY) and each live
// entry after. It receives the raw JSON bytes of the WAL entry.
func DialReplica(primaryAddr string, applyFn func([]byte) error) (*ReplicaConn, error) {
	conn, err := net.Dial("tcp", primaryAddr)
	if err != nil {
		return nil, fmt.Errorf("replica: dial %s: %w", primaryAddr, err)
	}

	if _, err := fmt.Fprintf(conn, "REPLICATE\n"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("replica: send REPLICATE: %w", err)
	}

	r := bufio.NewReader(conn)

	// Consume snapshot entries until +READY
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("replica: reading snapshot: %w", err)
		}
		line = strings.TrimRight(line, "\n")
		if line == "+READY" {
			break
		}
		if !strings.HasPrefix(line, "+") {
			conn.Close()
			return nil, fmt.Errorf("replica: unexpected line during snapshot: %q", line)
		}
		entry := []byte(line[1:])
		if err := applyFn(entry); err != nil {
			conn.Close()
			return nil, fmt.Errorf("replica: apply snapshot entry: %w", err)
		}
	}

	return &ReplicaConn{conn: conn, r: r}, nil
}

// Stream reads live WAL entries from the primary and calls applyFn for each.
// Blocks until the connection is closed or an error occurs.
func (rc *ReplicaConn) Stream(applyFn func([]byte) error) error {
	for {
		line, err := rc.r.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, "+") {
			continue
		}
		if err := applyFn([]byte(line[1:])); err != nil {
			return err
		}
	}
}

// Close closes the underlying connection.
func (rc *ReplicaConn) Close() error {
	return rc.conn.Close()
}
