package brisedb

import (
	"testing"
	"time"
)

func TestPubSub_PublishNoSubscribers(t *testing.T) {
	ps := newPubSub()
	if n := ps.Publish("news", "hello"); n != 0 {
		t.Errorf("want 0 receivers, got %d", n)
	}
}

func TestPubSub_SubscribeAndReceive(t *testing.T) {
	ps := newPubSub()
	ch := make(chan Message, 4)
	ps.Subscribe(ch, "news")

	n := ps.Publish("news", "hello")
	if n != 1 {
		t.Fatalf("want 1 receiver, got %d", n)
	}

	select {
	case msg := <-ch:
		if msg.Channel != "news" || msg.Payload != "hello" {
			t.Errorf("unexpected message: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPubSub_MultipleSubscribers(t *testing.T) {
	ps := newPubSub()
	ch1, ch2 := make(chan Message, 4), make(chan Message, 4)
	ps.Subscribe(ch1, "sport")
	ps.Subscribe(ch2, "sport")

	if n := ps.Publish("sport", "goal"); n != 2 {
		t.Fatalf("want 2 receivers, got %d", n)
	}
	if len(ch1) != 1 || len(ch2) != 1 {
		t.Error("both subscribers should have received the message")
	}
}

func TestPubSub_Unsubscribe(t *testing.T) {
	ps := newPubSub()
	ch := make(chan Message, 4)
	ps.Subscribe(ch, "news")
	ps.Unsubscribe(ch, "news")

	if n := ps.Publish("news", "late"); n != 0 {
		t.Errorf("want 0 receivers after unsubscribe, got %d", n)
	}
}

func TestPubSub_UnsubscribeAll(t *testing.T) {
	ps := newPubSub()
	ch := make(chan Message, 4)
	ps.Subscribe(ch, "a")
	ps.Subscribe(ch, "b")
	ps.UnsubscribeAll(ch)

	if n := ps.Publish("a", "x"); n != 0 {
		t.Errorf("channel a: want 0, got %d", n)
	}
	if n := ps.Publish("b", "x"); n != 0 {
		t.Errorf("channel b: want 0, got %d", n)
	}
}

func TestPubSub_NumSubscribers(t *testing.T) {
	ps := newPubSub()
	ch1, ch2 := make(chan Message, 1), make(chan Message, 1)

	if n := ps.NumSubscribers("x"); n != 0 {
		t.Errorf("want 0 before subscribe, got %d", n)
	}
	ps.Subscribe(ch1, "x")
	if n := ps.NumSubscribers("x"); n != 1 {
		t.Errorf("want 1, got %d", n)
	}
	ps.Subscribe(ch2, "x")
	if n := ps.NumSubscribers("x"); n != 2 {
		t.Errorf("want 2, got %d", n)
	}
	ps.Unsubscribe(ch1, "x")
	if n := ps.NumSubscribers("x"); n != 1 {
		t.Errorf("want 1 after unsub, got %d", n)
	}
}

func TestPubSub_PublishOnlyToCorrectChannel(t *testing.T) {
	ps := newPubSub()
	sports := make(chan Message, 4)
	news := make(chan Message, 4)
	ps.Subscribe(sports, "sports")
	ps.Subscribe(news, "news")

	ps.Publish("sports", "goal")

	if len(sports) != 1 {
		t.Error("sports subscriber should have 1 message")
	}
	if len(news) != 0 {
		t.Error("news subscriber should have 0 messages")
	}
}
