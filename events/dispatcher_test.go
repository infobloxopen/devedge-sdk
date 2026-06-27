package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// TestAC2_DispatchAtLeastOnceAndIdempotent proves the forward-cursor dispatcher
// delivers an event, that a handler failure leaves the cursor un-advanced so the event
// is re-delivered (at-least-once), and that once it succeeds the cursor advances past
// it so a later poll does NOT re-deliver — all WITHOUT ever mutating the write-only
// outbox row.
func TestAC2_DispatchAtLeastOnceAndIdempotent(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(w *widget) string { return w.ID })
	store := persistence.NewMemoryOutboxStore()
	cursors := persistence.NewMemoryOutboxCursorStore()
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
	d := events.NewDispatcher(store, cursors, tx, events.NewMemoryIdempotencyStore())
	d.Subscribe("Thing", "counter", func(ctx context.Context, evt events.Event) error {
		calls++
		if failOnce {
			failOnce = false
			return errors.New("transient handler failure")
		}
		applied++
		return nil
	})

	// First run: the handler fails, the cursor does not advance (at-least-once).
	delivered, err := d.RunOnce(ctx, 10)
	if err == nil {
		t.Fatal("expected the handler failure to surface")
	}
	if delivered != 0 {
		t.Fatalf("a failed handler must not advance the cursor, delivered=%d", delivered)
	}
	if c, _ := d.Cursor(ctx); !c.IsZero() {
		t.Fatalf("a failed delivery must leave the cursor at the start, got %+v", c)
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

	// Third run: the cursor has advanced past the event, so nothing is re-delivered and
	// the handler body is not re-invoked.
	callsBefore := calls
	delivered, err = d.RunOnce(ctx, 10)
	if err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("a consumed event must NOT be re-delivered on a later poll, delivered=%d", delivered)
	}
	if calls != callsBefore {
		t.Fatalf("a consumed event must not re-invoke the handler body, calls went %d -> %d", callsBefore, calls)
	}
	if applied != 1 {
		t.Fatalf("idempotency must keep the side effect at exactly one, applied=%d", applied)
	}

	// Write-only: the dispatch path NEVER mutated or deleted the outbox row — it only
	// advanced its sidecar cursor.
	got := store.All()
	if len(got) != 1 {
		t.Fatalf("write-only: the dispatch path must never delete the row, rows=%d", len(got))
	}
	if c, _ := d.Cursor(ctx); c.ID != "evt-1" {
		t.Fatalf("the cursor must have advanced to evt-1, got %+v", c)
	}
}

// TestAC2_RedeliveryIsNoOpViaIdempotency proves the exactly-once guard for the window
// the cursor does NOT close: a crash BETWEEN deliver() and the cursor-advance. The
// realistic race is a dispatcher that ran the handler (committing its idempotency
// marker) but crashed before saving the advanced cursor, so the next RunOnce re-reads
// the same event (the cursor is still behind it) and must dedup it via the marker. The
// recorded idempotency marker — not the cursor — is what keeps the side effect at
// exactly one across that redelivery.
//
// We simulate the crash window by recording the marker out-of-band (as the handler tx
// would) WITHOUT advancing the cursor, so the dispatcher's next RunOnce re-reads the
// same un-consumed event and must dedup it.
func TestAC2_RedeliveryIsNoOpViaIdempotency(t *testing.T) {
	repo := persistence.NewMemoryRepository(func(w *widget) string { return w.ID })
	store := persistence.NewMemoryOutboxStore()
	cursors := persistence.NewMemoryOutboxCursorStore()
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
	d := events.NewDispatcher(store, cursors, tx, idem)
	d.Subscribe("Thing", "effect", func(ctx context.Context, evt events.Event) error {
		sideEffects++
		return nil
	})

	// Out-of-band first delivery WITHOUT a cursor-advance: record the marker (as the
	// handler tx would) but never advance the cursor — the crash-before-advance window.
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return idem.Record(ctx, events.IdempotencyKeyForTest("dup-1", "effect"))
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	// The dispatcher re-reads the still-un-consumed event (cursor behind it) and must
	// dedup it via the recorded marker — the handler body does NOT run again.
	if _, err := d.RunOnce(ctx, 10); err != nil {
		t.Fatalf("re-deliver: %v", err)
	}
	if sideEffects != 0 {
		t.Fatalf("idempotency must suppress the side effect on a re-delivery, got %d", sideEffects)
	}
	// Having now fully delivered (marker present for every handler), the dispatcher
	// advanced its cursor past the event.
	if c, _ := d.Cursor(ctx); c.ID != "dup-1" {
		t.Fatalf("after a deduped delivery the cursor must advance past the event, got %+v", c)
	}
	if got := store.All(); len(got) != 1 {
		t.Fatalf("write-only: the row must remain (never deleted), rows=%d", len(got))
	}
}

// TestPoisonDeadLettersAndAdvances proves the head-of-line poison handling: an
// always-failing head event blocks the cursor for maxAttempts attempts (bounded
// head-of-line blocking), then is recorded to the sidecar dead-letter and the cursor
// advances PAST it so the NEXT event is delivered. The write-only outbox row is never
// touched.
func TestPoisonDeadLettersAndAdvances(t *testing.T) {
	store := persistence.NewMemoryOutboxStore()
	cursors := persistence.NewMemoryOutboxCursorStore()
	tx := persistence.NewMemoryTxRunner(store)
	pub := events.NewOutboxPublisher(store)
	ctx := context.Background()

	// Two events: a poison head and a good follower.
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		if err := pub.Publish(ctx, events.Event{ID: "poison", Type: "T"}); err != nil {
			return err
		}
		return pub.Publish(ctx, events.Event{ID: "good", Type: "T"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	const maxAttempts = 3
	poisonCalls, goodApplied := 0, 0
	d := events.NewDispatcher(store, cursors, tx, events.NewMemoryIdempotencyStore(), events.WithMaxAttempts(maxAttempts))
	d.Subscribe("T", "h", func(ctx context.Context, evt events.Event) error {
		if evt.ID == "poison" {
			poisonCalls++
			return errors.New("permanent failure")
		}
		goodApplied++
		return nil
	})

	// Drive the dispatcher repeatedly. The poison head blocks (head-of-line) for
	// maxAttempts, then is dead-lettered and the cursor advances; the good event then
	// delivers.
	for i := 0; i < maxAttempts+2; i++ {
		_, _ = d.RunOnce(ctx, 10)
	}

	if poisonCalls != maxAttempts {
		t.Fatalf("the poison head must be attempted exactly maxAttempts(%d) times, got %d", maxAttempts, poisonCalls)
	}
	if goodApplied != 1 {
		t.Fatalf("after dead-lettering the poison head the follower must deliver, applied=%d", goodApplied)
	}
	// The poison event was parked in the sidecar dead-letter (the outbox row untouched).
	dead := cursors.DeadLettered()
	if len(dead) != 1 || dead[0].EventID != "poison" {
		t.Fatalf("the poison event must be dead-lettered in the sidecar, got %+v", dead)
	}
	// Write-only: both rows survive in the outbox (the dispatch path never deletes).
	if all := store.All(); len(all) != 2 {
		t.Fatalf("write-only: the dispatch path must never delete a row, rows=%d", len(all))
	}
	// The cursor advanced past BOTH events.
	if c, _ := d.Cursor(ctx); c.ID != "good" {
		t.Fatalf("the cursor must end at the last event, got %+v", c)
	}
}

// TestDispatchRunsHandlerInItsOwnTx proves G-4: a handler observes a transactional
// context (RequireTx passes inside it), so a handler's aggregate write is atomic.
func TestDispatchRunsHandlerInItsOwnTx(t *testing.T) {
	pub, _, store, tx := setup()
	cursors := persistence.NewMemoryOutboxCursorStore()
	ctx := context.Background()
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "tx-1", Type: "Thing"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	sawTx := false
	d := events.NewDispatcher(store, cursors, tx, nil)
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
// handler must fail fast at Subscribe (a setup-time call) instead of nil-panicking on
// first delivery inside the poller goroutine — which would roll back, re-panic up
// through Poll, and silently crash all delivery without ever reaching onErr.
func TestSubscribeNilHandlerPanicsAtRegistration(t *testing.T) {
	_, _, store, tx := setup()
	cursors := persistence.NewMemoryOutboxCursorStore()
	d := events.NewDispatcher(store, cursors, tx, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("Subscribe with a nil handler must panic at registration")
		}
	}()
	d.Subscribe("Thing", "nil-handler", nil)
}

// TestIdempotencyKeyHasNoNULByte is the regression guard for a Postgres-fatal bug the
// Phase-2 PG validation surfaced: the (event, handler) idempotency key was joined with
// a NUL byte ("\x00"), which SQLite tolerates in a TEXT column but PostgreSQL rejects
// ("invalid byte sequence for encoding UTF8: 0x00", SQLSTATE 22021). On PG that made
// every Seen/Record query fail, so the exactly-once marker could never be stored and
// concurrent dispatch never converged. The key must therefore never contain a NUL byte
// — it has to round-trip through a Postgres text/varchar column.
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
