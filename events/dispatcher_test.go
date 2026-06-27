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
// succeeds it is NOT re-claimed on the next poll (the churn-free happy path).
//
// F033 (append-only, churn-free): on success the dispatcher stamps the row delivered
// ONCE (MarkDelivered), which excludes it from every future ClaimUndelivered — so a
// later poll does NOT re-lease or re-attempt the row (no per-poll write churn) and
// the handler body is not re-invoked. The row is never DELETEd (append-only). The
// idempotency marker is the exactly-once guard for an in-flight double-claim; here we
// assert the stronger no-re-claim property. The store uses a ~1ns lease so a still-
// undelivered (failed) event re-claims promptly.
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

	// Third run: the delivered row is NOT re-claimed (the churn-free happy path) — the
	// store excludes a delivered row from the claim, so nothing is delivered and the
	// handler body is not re-invoked. The row remains present (append-only).
	callsBefore := calls
	delivered, err = d.RunOnce(ctx, 10)
	if err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("a delivered event must NOT be re-claimed on a later poll, delivered=%d", delivered)
	}
	if calls != callsBefore {
		t.Fatalf("a delivered event must not re-invoke the handler body, calls went %d -> %d", callsBefore, calls)
	}
	if applied != 1 {
		t.Fatalf("idempotency must keep the side effect at exactly one, applied=%d", applied)
	}

	// Churn-free: a delivered row is no longer claim-eligible (no per-poll re-lease).
	if p := store.Pending(); len(p) != 0 {
		t.Fatalf("a delivered event must drop out of the claim set, pending=%v", p)
	}
	// Append-only: the dispatch path never deleted the row, only terminal-marked it.
	got := store.All()
	if len(got) != 1 {
		t.Fatalf("append-only: the dispatch path must never delete the row, rows=%d", len(got))
	}
	if got[0].DeliveredTime == nil {
		t.Fatal("a delivered row must carry a single terminal delivered-mark")
	}
}

// TestAC2_RedeliveryIsNoOpViaIdempotency proves the exactly-once guard for the one
// window the delivered-mark does NOT close: an in-flight double-claim. The realistic
// race is a dispatcher that ran the handler (committing its idempotency marker) but
// crashed BEFORE MarkDelivered, so the row's lease lapses and it is re-claimed still
// un-marked. The recorded idempotency marker — not the delivered-mark — is what keeps
// the side effect at exactly one across that redelivery.
//
// We simulate the crash window by claiming the row out-of-band (lease + attempt bump,
// no delivered-mark) so the dispatcher's next RunOnce re-claims the same un-delivered
// event after the ~1ns lease lapses and must dedup it via the marker.
func TestAC2_RedeliveryIsNoOpViaIdempotency(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(w *widget) string { return w.ID })
	store := persistence.NewMemoryOutboxStore(1) // ~1ns lease so a re-claim is allowed immediately
	tx := persistence.NewMemoryTxRunner(repo, store)
	pub := events.NewOutboxPublisher(store)
	idem := events.NewMemoryIdempotencyStore()
	ctx := context.Background()

	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "dup-1", Type: "Thing"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	sideEffects := 0
	d := events.NewDispatcher(store, tx, idem)
	d.Subscribe("Thing", "effect", func(ctx context.Context, evt events.Event) error {
		sideEffects++
		return nil
	})

	// Out-of-band first delivery WITHOUT a delivered-mark: record the marker (as the
	// handler tx would) but never call MarkDelivered — the crash-before-mark window.
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return idem.Record(ctx, events.IdempotencyKeyForTest("dup-1", "effect"))
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	// The dispatcher re-claims the still-un-delivered row (lease lapsed) and must
	// dedup it via the recorded marker — the handler body does NOT run again.
	if _, err := d.RunOnce(ctx, 10); err != nil {
		t.Fatalf("re-deliver: %v", err)
	}
	if sideEffects != 0 {
		t.Fatalf("idempotency must suppress the side effect on an in-flight double-claim, got %d", sideEffects)
	}
	// Having now fully delivered (marker present for every handler), the dispatcher
	// stamped the row delivered, so it drops out of future claims (churn-free).
	if p := store.Pending(); len(p) != 0 {
		t.Fatalf("after a deduped delivery the row must be marked delivered, pending=%v", p)
	}
	if got := store.All(); len(got) != 1 {
		t.Fatalf("append-only: the row must remain (never deleted), rows=%d", len(got))
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
