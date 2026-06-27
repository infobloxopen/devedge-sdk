package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// TestAC2_DispatchAtLeastOnceAndIdempotent proves the dispatcher delivers an
// undelivered event, that a handler crash leaves it for re-delivery (at-least-once),
// and that once it succeeds a duplicate delivery is a no-op via the idempotency key.
func TestAC2_DispatchAtLeastOnceAndIdempotent(t *testing.T) {
	pub, repo, store, tx := setup()
	ctx := context.Background()

	// Publish one event (inside a tx, the only legal way).
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		if _, err := repo.Create(ctx, &widget{ID: "w1"}); err != nil {
			return err
		}
		return pub.Publish(ctx, events.Event{ID: "evt-1", Type: "Thing", AggregateID: "w1"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// A handler that fails the FIRST time and succeeds afterwards, counting calls.
	calls := 0
	applied := 0
	failOnce := true
	d := events.NewDispatcher(store, tx, events.NewMemoryIdempotencyStore())
	d.Subscribe("Thing", "counter", func(ctx context.Context, evt events.Event) error {
		calls++
		if failOnce {
			failOnce = false
			return errors.New("transient handler failure")
		}
		applied++
		return nil
	})

	// First run: the handler fails, the event stays undelivered (at-least-once).
	delivered, err := d.RunOnce(ctx, 10)
	if err == nil {
		t.Fatal("expected the handler failure to surface")
	}
	if delivered != 0 {
		t.Fatalf("a failed handler must not mark the event delivered, delivered=%d", delivered)
	}
	if got := store.Pending(); len(got) != 1 {
		t.Fatalf("the event must remain pending after a handler failure, pending=%v", got)
	}

	// Second run: re-delivery succeeds and the event is marked delivered.
	delivered, err = d.RunOnce(ctx, 10)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("re-delivery must succeed exactly once, delivered=%d", delivered)
	}
	if applied != 1 {
		t.Fatalf("handler must have applied exactly once, applied=%d", applied)
	}
	if got := store.Pending(); len(got) != 0 {
		t.Fatalf("a delivered event must no longer be pending, pending=%v", got)
	}

	// Third run: nothing left to claim; the handler is NOT called again.
	callsBefore := calls
	delivered, err = d.RunOnce(ctx, 10)
	if err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("no undelivered events remain, delivered=%d", delivered)
	}
	if calls != callsBefore {
		t.Fatalf("a delivered event must not re-invoke the handler, calls went %d -> %d", callsBefore, calls)
	}
}

// flakyMarkStore wraps a MemoryOutboxStore and SWALLOWS the first MarkDelivered so
// the row stays undelivered — modelling the at-least-once double-fire window: the
// handler committed, but the process "crashed" before the delivered mark landed, so
// the same row is claimed and delivered a SECOND time on the next run.
type flakyMarkStore struct {
	*persistence.MemoryOutboxStore
	swallowedFirstMark bool
}

func (s *flakyMarkStore) MarkDelivered(ctx context.Context, id string) error {
	if !s.swallowedFirstMark {
		s.swallowedFirstMark = true
		return nil // pretend we crashed before persisting the mark
	}
	return s.MemoryOutboxStore.MarkDelivered(ctx, id)
}

// TestAC2_RedeliveryIsNoOpViaIdempotency proves that when the SAME event is claimed
// and delivered TWICE (the realistic case: the handler committed but the delivered
// mark was lost, so the row is re-claimed after its lease lapses), the idempotency
// key makes the second delivery a no-op — the handler's side effect runs once.
func TestAC2_RedeliveryIsNoOpViaIdempotency(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(w *widget) string { return w.ID })
	base := persistence.NewMemoryOutboxStore(1) // ~1ns lease so a re-claim is allowed immediately
	store := &flakyMarkStore{MemoryOutboxStore: base}
	tx := persistence.NewMemoryTxRunner(repo, base)
	pub := events.NewOutboxPublisher(store)
	ctx := context.Background()

	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "dup-1", Type: "Thing"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	sideEffects := 0
	d := events.NewDispatcher(store, tx, events.NewMemoryIdempotencyStore())
	d.Subscribe("Thing", "effect", func(ctx context.Context, evt events.Event) error {
		sideEffects++
		return nil
	})

	// First run: handler applies, but the delivered mark is swallowed → row remains.
	if _, err := d.RunOnce(ctx, 10); err != nil {
		t.Fatalf("first deliver: %v", err)
	}
	if sideEffects != 1 {
		t.Fatalf("handler must run once on first delivery, got %d", sideEffects)
	}
	if got := store.Pending(); len(got) != 1 {
		t.Fatalf("the swallowed mark must leave the row pending for re-delivery, got %v", got)
	}

	// Second run: the SAME event is re-claimed and re-delivered. The idempotency key
	// (already recorded) makes the handler a no-op; the mark now lands.
	if _, err := d.RunOnce(ctx, 10); err != nil {
		t.Fatalf("re-deliver: %v", err)
	}
	if sideEffects != 1 {
		t.Fatalf("idempotency must keep the side effect at exactly one across redelivery, got %d", sideEffects)
	}
	if got := store.Pending(); len(got) != 0 {
		t.Fatalf("the event must be delivered after re-delivery, pending=%v", got)
	}
}

// TestDispatchRunsHandlerInItsOwnTx proves G-4: a handler observes a transactional
// context (RequireTx passes inside it), so a handler's aggregate write is atomic.
func TestDispatchRunsHandlerInItsOwnTx(t *testing.T) {
	pub, _, store, tx := setup()
	ctx := context.Background()
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "tx-1", Type: "Thing"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	sawTx := false
	d := events.NewDispatcher(store, tx, nil)
	d.Subscribe("Thing", "checker", func(hctx context.Context, evt events.Event) error {
		sawTx = persistence.RequireTx(hctx) == nil
		return nil
	})
	if _, err := d.RunOnce(ctx, 10); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !sawTx {
		t.Fatal("a handler must run inside its own Atomically (RequireTx must pass)")
	}
}
