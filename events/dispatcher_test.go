package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// TestAC2_DispatchAtLeastOnceAndIdempotent proves the dispatcher delivers an event,
// that a handler crash leaves it for re-delivery (at-least-once), and that once it
// succeeds a duplicate delivery is a no-op via the idempotency key.
//
// F033 (append-only): delivery is NOT tracked by a store row write — the
// idempotency marker is the delivery truth. So a re-claim of an already-delivered
// row is expected; correctness is that the HANDLER's side effect runs exactly once
// (the recorded marker makes a re-claim a no-op via the Seen fast-path). The store
// uses a ~1ns lease so a failed event re-claims promptly.
func TestAC2_DispatchAtLeastOnceAndIdempotent(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(w *widget) string { return w.ID })
	store := persistence.NewMemoryOutboxStore(1) // ~1ns lease → re-claim allowed immediately
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)
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

	// First run: the handler fails, the event is NOT delivered (at-least-once).
	delivered, err := d.RunOnce(ctx, 10)
	if err == nil {
		t.Fatal("expected the handler failure to surface")
	}
	if delivered != 0 {
		t.Fatalf("a failed handler must not count as delivered, delivered=%d", delivered)
	}

	// Second run: re-delivery succeeds; the handler applies exactly once.
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

	// Third run: the row is still present (append-only), but its marker is recorded so
	// the handler is NOT re-invoked — the redelivery is a no-op (AC-2).
	callsBefore := calls
	delivered, err = d.RunOnce(ctx, 10)
	if err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	if delivered != 1 {
		// The event re-claims (append-only) and deliver() returns nil because every
		// handler's marker is already recorded — a delivered no-op, counted as delivered.
		t.Fatalf("an already-applied event delivers as a no-op, delivered=%d", delivered)
	}
	if calls != callsBefore {
		t.Fatalf("an applied event must not re-invoke the handler body, calls went %d -> %d", callsBefore, calls)
	}
	if applied != 1 {
		t.Fatalf("idempotency must keep the side effect at exactly one, applied=%d", applied)
	}

	// AC-1 (append-only): the dispatch path never deleted the row.
	if got := store.All(); len(got) != 1 {
		t.Fatalf("append-only: the dispatch path must never delete the row, rows=%d", len(got))
	}
}

// TestAC2_RedeliveryIsNoOpViaIdempotency proves that when the SAME event is claimed
// and delivered TWICE — the realistic at-least-once double-fire: the handler
// committed, then the row's lease lapsed and it was re-claimed — the idempotency key
// makes the second delivery a no-op, so the handler's side effect runs exactly once.
//
// F033 (append-only): the row is NOT marked delivered by the store (MarkDelivered is
// a no-op; the marker is the delivery truth), so with a ~1ns lease the row is always
// re-claimable. The recorded marker is what stops the side effect from firing twice.
func TestAC2_RedeliveryIsNoOpViaIdempotency(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(w *widget) string { return w.ID })
	store := persistence.NewMemoryOutboxStore(1) // ~1ns lease so a re-claim is allowed immediately
	tx := persistence.NewMemoryTxRunner(repo, store)
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

	// First run: handler applies; the marker is recorded. The append-only row remains.
	if _, err := d.RunOnce(ctx, 10); err != nil {
		t.Fatalf("first deliver: %v", err)
	}
	if sideEffects != 1 {
		t.Fatalf("handler must run once on first delivery, got %d", sideEffects)
	}
	if got := store.All(); len(got) != 1 {
		t.Fatalf("append-only: the row must remain after delivery, rows=%d", len(got))
	}

	// Second run: the SAME event re-claims (lease lapsed). The recorded idempotency
	// marker makes the handler a no-op — the side effect stays at exactly one.
	if _, err := d.RunOnce(ctx, 10); err != nil {
		t.Fatalf("re-deliver: %v", err)
	}
	if sideEffects != 1 {
		t.Fatalf("idempotency must keep the side effect at exactly one across redelivery, got %d", sideEffects)
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

// TestSubscribeNilHandlerPanicsAtRegistration guards the nil-handler footgun: a nil
// handler must fail fast at Subscribe (a setup-time call) instead of nil-panicking
// on first delivery inside the poller goroutine — which would roll back, re-panic up
// through Poll, and silently crash all delivery without ever reaching onErr.
func TestSubscribeNilHandlerPanicsAtRegistration(t *testing.T) {
	_, _, store, tx := setup()
	d := events.NewDispatcher(store, tx, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("Subscribe with a nil handler must panic at registration")
		}
	}()
	d.Subscribe("Thing", "nil-handler", nil)
}

// TestIdempotencyKeyHasNoNULByte is the regression guard for a Postgres-fatal bug
// the Phase-2 PG validation surfaced: the (event, handler) idempotency key was
// joined with a NUL byte ("\x00"), which SQLite tolerates in a TEXT column but
// PostgreSQL rejects ("invalid byte sequence for encoding UTF8: 0x00", SQLSTATE
// 22021). On PG that made every Seen/Record query fail, so the exactly-once marker
// could never be stored and concurrent dispatch never converged. The key must
// therefore never contain a NUL byte — it has to round-trip through a Postgres
// text/varchar column.
func TestIdempotencyKeyHasNoNULByte(t *testing.T) {
	for _, tc := range []struct{ eventID, handler string }{
		{"550e8400-e29b-41d4-a716-446655440000", "revoke-api-keys"},
		{"evt-1", "h"},
		{"", ""},
	} {
		key := events.IdempotencyKeyForTest(tc.eventID, tc.handler)
		for i := 0; i < len(key); i++ {
			if key[i] == 0x00 {
				t.Fatalf("idempotency key for (%q,%q) contains a NUL byte at %d — unstorable on PostgreSQL (SQLSTATE 22021)", tc.eventID, tc.handler, i)
			}
		}
		// The key must still distinguish the two components (the separator is present).
		if tc.eventID != "" && tc.handler != "" && len(key) <= len(tc.eventID)+len(tc.handler) {
			t.Fatalf("idempotency key %q must include a separator between event id and handler", key)
		}
	}
}
