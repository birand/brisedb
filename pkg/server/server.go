package server

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

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
	ps := s.db.PubSub()

	// msgCh receives pub/sub messages for this connection.
	msgCh := make(chan brisedb.Message, 256)
	// done signals the message-forwarder goroutine to stop.
	done := make(chan struct{})

	// subChannels tracks which channels this connection is subscribed to.
	subChannels := make(map[string]bool)

	scanner := bufio.NewScanner(conn)
	w := bufio.NewWriter(conn)
	var wmu sync.Mutex

	respond := func(msg string) {
		wmu.Lock()
		fmt.Fprintf(w, "%s\n", msg)
		w.Flush()
		wmu.Unlock()
	}

	// Forwarder: pushes received pub/sub messages to the client.
	go func() {
		for {
			select {
			case msg := <-msgCh:
				respond(fmt.Sprintf("+MESSAGE %s %s", msg.Channel, msg.Payload))
			case <-done:
				return
			}
		}
	}()

	defer func() {
		ps.UnsubscribeAll(msgCh)
		close(done)
	}()

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
			// SET key value [EX seconds]
			if len(args) < 2 {
				respond("-ERROR: SET requires key and value")
				continue
			}
			if len(args) >= 4 && strings.ToUpper(args[2]) == "EX" {
				secs, err := strconv.ParseInt(args[3], 10, 64)
				if err != nil || secs <= 0 {
					respond("-ERROR: EX requires a positive integer")
					continue
				}
				if err := session.SetEX(args[0], args[1], time.Duration(secs)*time.Second); err != nil {
					respond("-" + err.Error())
				} else {
					respond("+OK")
				}
			} else {
				if err := session.Set(args[0], args[1]); err != nil {
					respond("-" + err.Error())
				} else {
					respond("+OK")
				}
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
		case "TTL":
			if len(args) < 1 {
				respond("-ERROR: TTL requires key")
				continue
			}
			respond(fmt.Sprintf("+%d", session.TTL(args[0])))
		case "PERSIST":
			if len(args) < 1 {
				respond("-ERROR: PERSIST requires key")
				continue
			}
			if session.Persist(args[0]) {
				respond("+1")
			} else {
				respond("+0")
			}
		case "KEYS":
			if len(args) < 1 {
				respond("-ERROR: KEYS requires a pattern")
				continue
			}
			keys, err := session.Keys(args[0])
			if err != nil {
				respond("-" + err.Error())
				continue
			}
			respond(fmt.Sprintf("+%d", len(keys)))
			for _, k := range keys {
				respond("+" + k)
			}
		case "SCAN":
			// SCAN <cursor> COUNT <count>
			if len(args) < 1 {
				respond("-ERROR: SCAN requires a cursor")
				continue
			}
			cursor, err := strconv.Atoi(args[0])
			if err != nil || cursor < 0 {
				respond("-ERROR: cursor must be a non-negative integer")
				continue
			}
			count := 10
			if len(args) >= 3 && strings.ToUpper(args[1]) == "COUNT" {
				if n, err := strconv.Atoi(args[2]); err == nil && n > 0 {
					count = n
				}
			}
			next, keys := session.Scan(cursor, count)
			respond(fmt.Sprintf("+%d %d", next, len(keys)))
			for _, k := range keys {
				respond("+" + k)
			}
		case "SUBSCRIBE":
			if len(args) < 1 {
				respond("-ERROR: SUBSCRIBE requires at least one channel")
				continue
			}
			for _, ch := range args {
				if !subChannels[ch] {
					ps.Subscribe(msgCh, ch)
					subChannels[ch] = true
				}
				respond(fmt.Sprintf("+SUBSCRIBE %s %d", ch, len(subChannels)))
			}
		case "UNSUBSCRIBE":
			channels := args
			if len(channels) == 0 {
				// unsubscribe from all
				for ch := range subChannels {
					channels = append(channels, ch)
				}
			}
			for _, ch := range channels {
				if subChannels[ch] {
					ps.Unsubscribe(msgCh, ch)
					delete(subChannels, ch)
				}
				respond(fmt.Sprintf("+UNSUBSCRIBE %s %d", ch, len(subChannels)))
			}
		case "PUBLISH":
			if len(args) < 2 {
				respond("-ERROR: PUBLISH requires channel and message")
				continue
			}
			respond(fmt.Sprintf("+%d", ps.Publish(args[0], args[1])))
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
