package brisedb

import "sync"

// Message is a pub/sub message delivered to subscribers.
type Message struct {
	Channel string
	Payload string
}

// PubSub manages channel subscriptions. It is safe for concurrent use.
type PubSub struct {
	mu   sync.RWMutex
	subs map[string]map[chan Message]struct{}
}

func newPubSub() *PubSub {
	return &PubSub{subs: make(map[string]map[chan Message]struct{})}
}

// Subscribe registers ch to receive messages published to channel.
func (ps *PubSub) Subscribe(ch chan Message, channel string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.subs[channel] == nil {
		ps.subs[channel] = make(map[chan Message]struct{})
	}
	ps.subs[channel][ch] = struct{}{}
}

// Unsubscribe removes ch from channel.
func (ps *PubSub) Unsubscribe(ch chan Message, channel string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.subs[channel], ch)
	if len(ps.subs[channel]) == 0 {
		delete(ps.subs, channel)
	}
}

// UnsubscribeAll removes ch from every channel it is subscribed to.
func (ps *PubSub) UnsubscribeAll(ch chan Message) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for channel, subs := range ps.subs {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(ps.subs, channel)
		}
	}
}

// Publish sends payload to all subscribers of channel and returns the
// number of subscribers that received the message. Subscribers whose
// buffer is full are skipped (non-blocking send).
func (ps *PubSub) Publish(channel, payload string) int {
	// Collect subscribers under a read lock, then release before sending
	// to avoid holding the lock while a subscriber goroutine tries to unsub.
	ps.mu.RLock()
	subs := make([]chan Message, 0, len(ps.subs[channel]))
	for ch := range ps.subs[channel] {
		subs = append(subs, ch)
	}
	ps.mu.RUnlock()

	msg := Message{Channel: channel, Payload: payload}
	sent := 0
	for _, ch := range subs {
		select {
		case ch <- msg:
			sent++
		default:
			// subscriber buffer full — skip
		}
	}
	return sent
}

// NumSubscribers returns the number of active subscribers for channel.
func (ps *PubSub) NumSubscribers(channel string) int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.subs[channel])
}
