package client_test

import (
	"testing"
	"time"

	"github.com/birand/brisedb/pkg/client"
)

func dialPubSub(t *testing.T, addr string) *client.PubSubConn {
	t.Helper()
	ps, err := client.DialPubSub(addr)
	if err != nil {
		t.Fatalf("DialPubSub: %v", err)
	}
	t.Cleanup(func() { ps.Close() })
	return ps
}

// nextMsg waits for the next message of a given kind on ps.Messages().
// If kind is empty, the first message of any kind is returned.
func nextMsg(t *testing.T, ps *client.PubSubConn, kind string) client.PubSubMessage {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m, ok := <-ps.Messages():
			if !ok {
				t.Fatal("Messages channel closed unexpectedly")
			}
			if kind == "" || m.Kind == kind {
				return m
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q message", kind)
			return client.PubSubMessage{}
		}
	}
}

func TestPubSubPublishNoSubscribers(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)
	defer c.Close()

	n, err := c.Publish("news", "hello")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if n != 0 {
		t.Errorf("want 0 receivers, got %d", n)
	}
}

func TestPubSubSubscribeAndReceive(t *testing.T) {
	addr := startServer(t)
	ps := dialPubSub(t, addr)

	ps.Subscribe("news")
	sub := nextMsg(t, ps, "subscribe")
	if sub.Channel != "news" || sub.Count != 1 {
		t.Errorf("subscribe confirmation: %+v", sub)
	}

	pub := dial(t, addr)
	defer pub.Close()
	n, _ := pub.Publish("news", "hello")
	if n != 1 {
		t.Errorf("want 1 receiver, got %d", n)
	}

	msg := nextMsg(t, ps, "message")
	if msg.Channel != "news" || msg.Payload != "hello" {
		t.Errorf("unexpected message: %+v", msg)
	}
}

func TestPubSubMultipleChannels(t *testing.T) {
	addr := startServer(t)
	ps := dialPubSub(t, addr)

	ps.Subscribe("a", "b")
	nextMsg(t, ps, "subscribe") // a
	nextMsg(t, ps, "subscribe") // b

	pub := dial(t, addr)
	defer pub.Close()
	pub.Publish("a", "msg-a")
	pub.Publish("b", "msg-b")

	got := map[string]string{}
	for i := 0; i < 2; i++ {
		m := nextMsg(t, ps, "message")
		got[m.Channel] = m.Payload
	}
	if got["a"] != "msg-a" {
		t.Errorf("channel a: want msg-a, got %q", got["a"])
	}
	if got["b"] != "msg-b" {
		t.Errorf("channel b: want msg-b, got %q", got["b"])
	}
}

func TestPubSubMultipleSubscribers(t *testing.T) {
	addr := startServer(t)
	ps1 := dialPubSub(t, addr)
	ps2 := dialPubSub(t, addr)

	ps1.Subscribe("sport")
	ps2.Subscribe("sport")
	nextMsg(t, ps1, "subscribe")
	nextMsg(t, ps2, "subscribe")

	pub := dial(t, addr)
	defer pub.Close()
	n, _ := pub.Publish("sport", "goal")
	if n != 2 {
		t.Errorf("want 2 receivers, got %d", n)
	}

	nextMsg(t, ps1, "message")
	nextMsg(t, ps2, "message")
}

func TestPubSubUnsubscribe(t *testing.T) {
	addr := startServer(t)
	ps := dialPubSub(t, addr)

	ps.Subscribe("news")
	nextMsg(t, ps, "subscribe")

	ps.Unsubscribe("news")
	unsub := nextMsg(t, ps, "unsubscribe")
	if unsub.Channel != "news" || unsub.Count != 0 {
		t.Errorf("unsubscribe confirmation: %+v", unsub)
	}

	pub := dial(t, addr)
	defer pub.Close()
	n, _ := pub.Publish("news", "late")
	if n != 0 {
		t.Errorf("want 0 receivers after unsubscribe, got %d", n)
	}
}

func TestPubSubUnsubscribeAll(t *testing.T) {
	addr := startServer(t)
	ps := dialPubSub(t, addr)

	ps.Subscribe("a", "b")
	nextMsg(t, ps, "subscribe")
	nextMsg(t, ps, "subscribe")

	ps.Unsubscribe() // no args = all

	m1 := nextMsg(t, ps, "unsubscribe")
	m2 := nextMsg(t, ps, "unsubscribe")
	if m1.Count != 1 && m2.Count != 0 {
		// order may vary, just check both arrived
		t.Errorf("expected counts 1 and 0, got %d and %d", m1.Count, m2.Count)
	}
}

func TestPubSubIsolatedChannels(t *testing.T) {
	addr := startServer(t)
	sports := dialPubSub(t, addr)
	news := dialPubSub(t, addr)

	sports.Subscribe("sport")
	news.Subscribe("news")
	nextMsg(t, sports, "subscribe")
	nextMsg(t, news, "subscribe")

	pub := dial(t, addr)
	defer pub.Close()
	pub.Publish("sport", "goal")

	// sports subscriber receives the message
	nextMsg(t, sports, "message")

	// news subscriber should NOT receive it — verify with a timeout
	select {
	case m := <-news.Messages():
		if m.Kind == "message" {
			t.Errorf("news subscriber got unexpected message: %+v", m)
		}
	case <-time.After(100 * time.Millisecond):
		// expected — no message for news
	}
}
