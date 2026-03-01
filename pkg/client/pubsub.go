package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
)

// PubSubMessage is a message received over a pub/sub connection.
type PubSubMessage struct {
	// Kind is one of "subscribe", "unsubscribe", or "message".
	Kind    string
	Channel string
	// Payload is set for Kind == "message".
	Payload string
	// Count is the number of active subscriptions after this event.
	// Set for Kind == "subscribe" and "unsubscribe".
	Count int
}

// PubSubConn is a dedicated connection for pub/sub.
// It must not be used for regular key-value commands.
//
// Usage:
//
//	ps, _ := client.DialPubSub("localhost:6380")
//	defer ps.Close()
//	ps.Subscribe("news", "sports")
//	for msg := range ps.Messages() {
//	    fmt.Println(msg.Channel, msg.Payload)
//	}
type PubSubConn struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex
	msgs chan PubSubMessage
	once sync.Once
	done chan struct{}
}

// DialPubSub opens a dedicated pub/sub connection to addr.
func DialPubSub(addr string) (*PubSubConn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("brisedb pubsub: dial %s: %w", addr, err)
	}
	p := &PubSubConn{
		conn: conn,
		r:    bufio.NewReader(conn),
		msgs: make(chan PubSubMessage, 256),
		done: make(chan struct{}),
	}
	go p.readLoop()
	return p, nil
}

// Subscribe registers this connection as a subscriber for channels.
// Confirmations arrive as PubSubMessage{Kind: "subscribe"} in Messages().
func (p *PubSubConn) Subscribe(channels ...string) error {
	return p.send("SUBSCRIBE", channels...)
}

// Unsubscribe removes this connection from channels.
// Passing no channels unsubscribes from all.
// Confirmations arrive as PubSubMessage{Kind: "unsubscribe"} in Messages().
func (p *PubSubConn) Unsubscribe(channels ...string) error {
	return p.send("UNSUBSCRIBE", channels...)
}

// Messages returns a channel of incoming pub/sub events.
// The channel is closed when the connection is closed.
func (p *PubSubConn) Messages() <-chan PubSubMessage {
	return p.msgs
}

// Close closes the connection and drains Messages().
func (p *PubSubConn) Close() error {
	p.once.Do(func() {
		close(p.done)
		p.conn.Close()
	})
	return nil
}

func (p *PubSubConn) send(cmd string, args ...string) error {
	parts := append([]string{cmd}, args...)
	line := strings.Join(parts, " ") + "\n"
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err := p.conn.Write([]byte(line))
	return err
}

// readLoop runs in a goroutine and parses all incoming lines into PubSubMessage.
func (p *PubSubConn) readLoop() {
	defer close(p.msgs)
	for {
		line, err := p.r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\n")
		msg, ok := parsePubSubLine(line)
		if !ok {
			continue
		}
		select {
		case p.msgs <- msg:
		case <-p.done:
			return
		}
	}
}

// parsePubSubLine parses a server response line into a PubSubMessage.
// Expected formats:
//
//	+MESSAGE <channel> <payload>
//	+SUBSCRIBE <channel> <count>
//	+UNSUBSCRIBE <channel> <count>
func parsePubSubLine(line string) (PubSubMessage, bool) {
	if !strings.HasPrefix(line, "+") {
		return PubSubMessage{}, false
	}
	parts := strings.SplitN(line[1:], " ", 3)
	if len(parts) < 1 {
		return PubSubMessage{}, false
	}
	kind := strings.ToLower(parts[0])
	switch kind {
	case "message":
		if len(parts) < 3 {
			return PubSubMessage{}, false
		}
		return PubSubMessage{Kind: "message", Channel: parts[1], Payload: parts[2]}, true
	case "subscribe", "unsubscribe":
		if len(parts) < 3 {
			return PubSubMessage{}, false
		}
		var count int
		fmt.Sscan(parts[2], &count)
		return PubSubMessage{Kind: kind, Channel: parts[1], Count: count}, true
	}
	return PubSubMessage{}, false
}
