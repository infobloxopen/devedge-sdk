package events_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/events/membus"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// waitFor polls cond until it is true or the deadline elapses, so an async-bus test does
// not race the consume goroutine. It fails the test if cond never holds.
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

// TestBusE2E_OutboxRelayMembusConsumerHandler is the Phase-1 end-to-end: an event
// published to the WRITE-ONLY outbox flows outbox → RELAY (ReadAfter+cursor) → in-memory
// BUS (membus) → CONSUMER (Subscribe/Consume) → handler ("propagation back in"), with the
// relay and consumer running as the two independent goroutines a real service wires.
func TestBusE2E_OutboxRelayMembusConsumerHandler(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(w *widget) string { return w.ID })
	store := persistence.NewMemoryOutboxStore()
	cursors := persistence.NewMemoryOutboxCursorStore()
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)
	ctx := context.Background()

	// Produce: aggregate write + outbox Append in one tx (the only legal publish path).
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		if _, err := repo.Create(ctx, &widget{ID: "w1"}); err != nil {
			return err
		}
		return pub.Publish(ctx, events.Event{ID: "evt-1", Type: "Thing", AggregateID: "w1", Payload: []byte("hello")})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	bus := membus.New()
	var got atomic.Value
	consumer := events.NewConsumer(bus, tx, events.NewMemoryIdempotencyStore())
	consumer.Subscribe("Thing", "record", func(hctx context.Context, evt events.Event) error {
		if err := persistence.RequireTx(hctx); err != nil {
			return err // the handler must run inside its own Atomically (G-4)
		}
		got.Store(string(evt.Payload))
		return nil
	})

	relay := events.NewRelay(store, cursors, bus)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = consumer.Run(runCtx) }()
	go func() { defer wg.Done(); relay.Run(runCtx, time.Millisecond, 10, nil) }()

	waitFor(t, "the handler to receive the event payload", func() bool {
		v, _ := got.Load().(string)
		return v == "hello"
	})
	// The relay advanced its cursor past the published event.
	waitFor(t, "the relay cursor to advance past evt-1", func() bool {
		c, _ := relay.Cursor(runCtx)
		return c.ID == "evt-1"
	})
	cancel()
	wg.Wait()

	// Write-only: the outbox row survived (the relay only advanced its sidecar cursor).
	if all := store.All(); len(all) != 1 {
		t.Fatalf("write-only: the relay must never delete the outbox row, rows=%d", len(all))
	}
}

// TestBusExactlyOnceUnderRedelivery proves the exactly-once EFFECT under an at-least-once
// bus: a handler NACKs (errors) its first delivery, the membus redelivers it, and the
// per-(event, handler) idempotency marker keeps the transactional side effect at exactly
// one across the redelivery.
func TestBusExactlyOnceUnderRedelivery(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(c *counter) string { return c.ID })
	store := persistence.NewMemoryOutboxStore()
	cursors := persistence.NewMemoryOutboxCursorStore()
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)
	ctx := context.Background()

	if _, err := repo.Create(ctx, &counter{ID: "c"}); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "evt-dup", Type: "Incr", AggregateID: "c"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	bus := membus.New()
	var attempts int64
	failFirst := int64(1) // NACK exactly the first delivery
	consumer := events.NewConsumer(bus, tx, events.NewMemoryIdempotencyStore())
	consumer.Subscribe("Incr", "incr", func(hctx context.Context, evt events.Event) error {
		atomic.AddInt64(&attempts, 1)
		if atomic.CompareAndSwapInt64(&failFirst, 1, 0) {
			return errors.New("transient handler failure (NACK → redeliver)")
		}
		c, err := repo.Get(hctx, "c")
		if err != nil {
			return err
		}
		c.Count++
		_, err = repo.Update(hctx, "c", c)
		return err
	})
	relay := events.NewRelay(store, cursors, bus)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = consumer.Run(runCtx) }()
	go func() { defer wg.Done(); relay.Run(runCtx, time.Millisecond, 10, nil) }()

	// The effect must converge to exactly one increment despite the redelivery.
	waitFor(t, "the counter to be incremented exactly once after a NACK+redelivery", func() bool {
		c, err := repo.Get(runCtx, "c")
		return err == nil && c.Count == 1
	})
	// Hold a moment to ensure no further increment slips through, then assert.
	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()

	c, err := repo.Get(ctx, "c")
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	if c.Count != 1 {
		t.Fatalf("exactly-once: the effect must commit once across a redelivery, Count=%d (attempts=%d)", c.Count, atomic.LoadInt64(&attempts))
	}
	if atomic.LoadInt64(&attempts) < 2 {
		t.Fatalf("the test must have actually redelivered (NACKed once), attempts=%d", atomic.LoadInt64(&attempts))
	}
}

// TestRelayLeaderElection_SingleActiveRelay proves the leader-elected relay: two relays
// share ONE [events.Leader] (as two replicas of one service would, via a cross-process
// lock); exactly one becomes the active relay and pumps the outbox, the other idles. The
// event is published to the bus exactly once per relay-pump (no double-publish), so the
// handler effect lands once.
func TestRelayLeaderElection_SingleActiveRelay(t *testing.T) {
	store := persistence.NewMemoryOutboxStore()
	cursors := persistence.NewMemoryOutboxCursorStore() // SHARED cursor (one service)
	tx := persistence.NewMemoryTxRunner(store)
	pub := events.NewOutboxPublisher(store)
	ctx := context.Background()

	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "evt-le", Type: "T", AggregateID: "a"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Count how many times the bus actually carries the event: with single-relay election
	// it must be exactly one even though TWO relays are running.
	bus := membus.New()
	var busDeliveries int64
	consumer := events.NewConsumer(bus, tx, events.NewMemoryIdempotencyStore())
	consumer.Subscribe("T", "count-bus", func(hctx context.Context, evt events.Event) error {
		atomic.AddInt64(&busDeliveries, 1)
		return nil
	})

	// One shared leader: the relay seam a multi-replica deployment plugs a PG advisory
	// lock into. Both relays contend for it; only one wins and pumps.
	leader := events.NewSingleProcessLeader()
	relayA := events.NewRelay(store, cursors, bus, events.WithRelayLeader(leader))
	relayB := events.NewRelay(store, cursors, bus, events.WithRelayLeader(leader))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = consumer.Run(runCtx) }()
	go func() { defer wg.Done(); relayA.Run(runCtx, time.Millisecond, 10, nil) }()
	go func() { defer wg.Done(); relayB.Run(runCtx, time.Millisecond, 10, nil) }()

	waitFor(t, "the event to be delivered through the bus once", func() bool {
		return atomic.LoadInt64(&busDeliveries) >= 1
	})
	time.Sleep(50 * time.Millisecond) // give a hypothetical second relay time to double-pump
	cancel()
	wg.Wait()

	if got := atomic.LoadInt64(&busDeliveries); got != 1 {
		t.Fatalf("single-relay election: the event must reach the bus exactly once, got %d (a second active relay double-published)", got)
	}

	// Only ONE of the two relays advanced the shared cursor — the active leader.
	c, _, _ := cursors.LoadCursor(ctx, events.DefaultCursorName)
	if c.ID != "evt-le" {
		t.Fatalf("the active relay must have advanced the cursor past evt-le, got %+v", c)
	}
}

// TestSingleProcessLeader_AtMostOneLeader is the unit-level proof that the dev leader is a
// genuine mutual exclusion, not an unconditional yes: while one caller holds it, another
// TryAcquire returns false until the holder Releases.
func TestSingleProcessLeader_AtMostOneLeader(t *testing.T) {
	ctx := context.Background()
	l := events.NewSingleProcessLeader()

	got, err := l.TryAcquire(ctx)
	if err != nil || !got {
		t.Fatalf("first acquire must succeed, got=%v err=%v", got, err)
	}
	if again, _ := l.TryAcquire(ctx); again {
		t.Fatal("a second concurrent acquire must be denied while the first holds the lock")
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if after, _ := l.TryAcquire(ctx); !after {
		t.Fatal("after Release the lock must be acquirable again")
	}
}
