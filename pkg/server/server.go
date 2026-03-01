package server

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/birand/brisedb/pkg/brisedb"
)

// Server accepts TCP connections and handles brisedb commands per connection.
type Server struct {
	db       *brisedb.BriseDB
	listener net.Listener
	wg       sync.WaitGroup
}

// New creates a Server bound to addr.
func New(db *brisedb.BriseDB, addr string) (*Server, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return &Server{db: db, listener: l}, nil
}

// Start accepts connections until Shutdown is called.
func (s *Server) Start() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// listener closed — normal shutdown
			return nil
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// Shutdown closes the listener and waits for all connections to finish.
func (s *Server) Shutdown() error {
	err := s.listener.Close()
	s.wg.Wait()
	return err
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	session := s.db.NewSession()
	scanner := bufio.NewScanner(conn)
	w := bufio.NewWriter(conn)

	respond := func(msg string) {
		fmt.Fprintf(w, "%s\n", msg)
		w.Flush()
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		cmd := strings.ToUpper(parts[0])
		args := parts[1:]

		switch cmd {
		case "SET":
			if len(args) < 2 {
				respond("-ERROR: SET requires key and value")
				continue
			}
			if err := session.Set(args[0], args[1]); err != nil {
				respond("-" + err.Error())
			} else {
				respond("+OK")
			}
		case "GET":
			if len(args) < 1 {
				respond("-ERROR: GET requires key")
				continue
			}
			if val, ok := session.Get(args[0]); ok {
				respond("+" + val)
			} else {
				respond("+nil")
			}
		case "DELETE":
			if len(args) < 1 {
				respond("-ERROR: DELETE requires key")
				continue
			}
			if err := session.Delete(args[0]); err != nil {
				respond("-" + err.Error())
			} else {
				respond("+OK")
			}
		case "COUNT":
			if len(args) < 1 {
				respond("-ERROR: COUNT requires value")
				continue
			}
			respond(fmt.Sprintf("+%d", session.Count(args[0])))
		case "BEGIN":
			session.BeginTransaction()
			respond("+OK")
		case "COMMIT":
			if err := session.CommitTransaction(); err != nil {
				respond("-" + err.Error())
			} else {
				respond("+OK")
			}
		case "ROLLBACK", "END":
			if err := session.RollbackTransaction(); err != nil {
				respond("-" + err.Error())
			} else {
				respond("+OK")
			}
		case "COMPACT":
			if err := s.db.Compact(); err != nil {
				respond("-" + err.Error())
			} else {
				respond("+OK")
			}
		case "STOP":
			respond("+OK")
			return
		default:
			respond("-ERROR: unknown command " + cmd)
		}
	}
}
