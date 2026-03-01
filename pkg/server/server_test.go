package server

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/birand/brisedb/pkg/brisedb"
)

// startTestServer spins up a server on a random port and returns a cleanup func.
func startTestServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	walPath := t.TempDir() + "/wal.log"
	db, err := brisedb.NewBriseDB(walPath)
	if err != nil {
		t.Fatalf("NewBriseDB: %v", err)
	}
	srv, err := New(db, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go srv.Start()
	return srv.Addr(), func() {
		srv.Shutdown()
		db.Close()
	}
}

// client dials the server and returns a send/recv helper plus a close func.
func client(t *testing.T, addr string) (send func(string) string, close func()) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	r := bufio.NewReader(conn)
	sendFn := func(cmd string) string {
		fmt.Fprintf(conn, "%s\n", cmd)
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read response to %q: %v", cmd, err)
		}
		return strings.TrimRight(line, "\n")
	}
	return sendFn, func() { conn.Close() }
}

func TestServerSetGet(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	if got := send("SET name Alice"); got != "+OK" {
		t.Errorf("SET: want +OK, got %q", got)
	}
	if got := send("GET name"); got != "+Alice" {
		t.Errorf("GET: want +Alice, got %q", got)
	}
}

func TestServerGetMissing(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	if got := send("GET missing"); got != "+nil" {
		t.Errorf("GET missing: want +nil, got %q", got)
	}
}

func TestServerDelete(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	send("SET x 1")
	if got := send("DELETE x"); got != "+OK" {
		t.Errorf("DELETE: want +OK, got %q", got)
	}
	if got := send("GET x"); got != "+nil" {
		t.Errorf("GET after DELETE: want +nil, got %q", got)
	}
}

func TestServerCount(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	send("SET a foo")
	send("SET b foo")
	send("SET c bar")

	if got := send("COUNT foo"); got != "+2" {
		t.Errorf("COUNT foo: want +2, got %q", got)
	}
	if got := send("COUNT bar"); got != "+1" {
		t.Errorf("COUNT bar: want +1, got %q", got)
	}
	if got := send("COUNT baz"); got != "+0" {
		t.Errorf("COUNT baz: want +0, got %q", got)
	}
}

func TestServerTransaction_Commit(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	send("BEGIN")
	send("SET k v")
	if got := send("COMMIT"); got != "+OK" {
		t.Errorf("COMMIT: want +OK, got %q", got)
	}
	if got := send("GET k"); got != "+v" {
		t.Errorf("GET after COMMIT: want +v, got %q", got)
	}
}

func TestServerTransaction_Rollback(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	send("BEGIN")
	send("SET k v")
	if got := send("ROLLBACK"); got != "+OK" {
		t.Errorf("ROLLBACK: want +OK, got %q", got)
	}
	if got := send("GET k"); got != "+nil" {
		t.Errorf("GET after ROLLBACK: want +nil, got %q", got)
	}
}

func TestServerTransaction_IsolatedSessions(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	// Two independent clients — each has its own transaction context.
	send1, close1 := client(t, addr)
	defer close1()
	send2, close2 := client(t, addr)
	defer close2()

	send1("BEGIN")
	send1("SET shared 1")

	// Client 2 should not see the uncommitted write from client 1.
	if got := send2("GET shared"); got != "+nil" {
		t.Errorf("isolation: client2 saw uncommitted write, got %q", got)
	}

	send1("COMMIT")

	// After commit, client 2 sees the value.
	if got := send2("GET shared"); got != "+1" {
		t.Errorf("after commit: client2 want +1, got %q", got)
	}
}

func TestServerCommitWithNoTransaction(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	if got := send("COMMIT"); !strings.HasPrefix(got, "-") {
		t.Errorf("COMMIT with no tx: want error, got %q", got)
	}
}

func TestServerRollbackWithNoTransaction(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	if got := send("ROLLBACK"); !strings.HasPrefix(got, "-") {
		t.Errorf("ROLLBACK with no tx: want error, got %q", got)
	}
}

func TestServerUnknownCommand(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	if got := send("FOOBAR"); !strings.HasPrefix(got, "-") {
		t.Errorf("unknown command: want error, got %q", got)
	}
}

func TestServerMissingArgs(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	cases := []string{"SET key", "GET", "DELETE", "COUNT"}
	for _, cmd := range cases {
		if got := send(cmd); !strings.HasPrefix(got, "-") {
			t.Errorf("%q: want error response, got %q", cmd, got)
		}
	}
}

func TestServerStop(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	send, close := client(t, addr)
	defer close()

	if got := send("STOP"); got != "+OK" {
		t.Errorf("STOP: want +OK, got %q", got)
	}
}

func TestServerConcurrentClients(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	const n = 20
	errs := make(chan string, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			send, close := client(t, addr)
			defer close()
			key := fmt.Sprintf("key%d", i)
			val := fmt.Sprintf("val%d", i)
			send(fmt.Sprintf("SET %s %s", key, val))
			got := send(fmt.Sprintf("GET %s", key))
			if got != "+"+val {
				errs <- fmt.Sprintf("client %d: want +%s, got %q", i, val, got)
			} else {
				errs <- ""
			}
		}(i)
	}

	for i := 0; i < n; i++ {
		if msg := <-errs; msg != "" {
			t.Error(msg)
		}
	}
}
