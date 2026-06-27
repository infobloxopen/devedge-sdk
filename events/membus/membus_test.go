package membus_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/events/membus"
)

func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", why)
}

// TestPublishSubscribeDelivers proves the basic in-process Publish→Consume path: a
// message published to a topic reaches a subscribed handler.
func TestPublishSubscribeDelivers(t *testing.T) {
	bus := membus.New()
	var got atomic.Value
	if err := bus.Subscribe("g1", "topic", func(ctx context.Context, msg events.BusMessage) error {
		got.Store(msg.Event.ID)
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Consume(ctx) }()

	if err := bus.Publish(ctx, "topic", events.BusMessage{Event: events.Event{ID: "e1"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "the message to be delivered", func() bool { v, _ := got.Load().(string); return v == "e1" })
}

// TestFanOutAcrossGroups proves broker-style fan-out: two DISTINCT consumer groups each
// receive their own copy of every message on a topic.
func TestFanOutAcrossGroups(t *testing.T) {
	bus := membus.New()
	var g1, g2 int64
	_ = bus.Subscribe("group-1", "topic", func(ctx context.Context, msg events.BusMessage) error { atomic.AddInt64(&g1, 1); return nil })
	_ = bus.Subscribe("group-2", "topic", func(ctx context.Context, msg events.BusMessage) error { atomic.AddInt64(&g2, 1); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Consume(ctx) }()

	if err := bus.Publish(ctx, "topic", events.BusMessage{Event: events.Event{ID: "e1"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "both groups to receive the message", func() bool {
		return atomic.LoadInt64(&g1) == 1 && atomic.LoadInt64(&g2) == 1
	})
}

// TestNackRedeliversInOrder proves the at-least-once contract and head-of-line ordering:
// a handler that NACKs (errors) the first message is redelivered the SAME message before
// the next message is delivered, so a later message never overtakes a failing one.
func TestNackRedeliversInOrder(t *testing.T) {
	bus := membus.New()
	var (
		mu    sync.Mutex
		order []string
		fail  = map[string]int{"e1": 1} // NACK e1 once
	)
	_ = bus.Subscribe("g", "topic", func(ctx context.Context, msg events.BusMessage) error {
		id := msg.Event.ID
		mu.Lock()
		defer mu.Unlock()
		if fail[id] > 0 {
			fail[id]--
			return errors.New("transient")
		}
		order = append(order, id)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Consume(ctx) }()

	for _, id := range []string{"e1", "e2"} {
		if err := bus.Publish(ctx, "topic", events.BusMessage{Event: events.Event{ID: id}}); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}
	waitFor(t, "both messages to be acked", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	// e1 NACKed once then redelivered+acked BEFORE e2 — order preserved (head-of-line).
	if len(order) != 2 || order[0] != "e1" || order[1] != "e2" {
		t.Fatalf("a NACKed message must be redelivered in order before the next, got %v", order)
	}
}

// TestPublishAfterCloseErrors proves Close makes further Publish fail with ErrBusClosed
// (a clean shutdown signal for the relay).
func TestPublishAfterCloseErrors(t *testing.T) {
	bus := membus.New()
	_ = bus.Subscribe("g", "topic", func(ctx context.Context, msg events.BusMessage) error { return nil })
	bus.Close()
	if err := bus.Publish(context.Background(), "topic", events.BusMessage{Event: events.Event{ID: "x"}}); !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Publish after Close must return ErrBusClosed, got %v", err)
	}
}
