package persistence_test

import (
	"context"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// appendTx appends rec to the store inside a one-shot MemoryTxRunner transaction the
// store is enrolled in (the only legal Append path — the dual-write guard).
func appendTx(t *testing.T, tx *persistence.MemoryTxRunner, store *persistence.MemoryOutboxStore, rec *persistence.OutboxRecord) {
	t.Helper()
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		return store.Append(ctx, rec)
	}); err != nil {
		t.Fatalf("append %s: %v", rec.ID, err)
	}
}

// TestMemoryOutbox_AppendOnly_ClaimNeverDeletes proves AC-1 on the memory dev store:
// claiming (and the no-op MarkDelivered) never delete or terminal-mark a row — the
// row count only grows until a partition drop.
func TestMemoryOutbox_AppendOnly_ClaimNeverDeletes(t *testing.T) {
	store := persistence.NewMemoryOutboxStore(time.Nanosecond) // tiny lease → always re-claimable
	tx := persistence.NewMemoryTxRunner(store)
	ctx := context.Background()

	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "a", EventType: "X"})
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "b", EventType: "X"})

	for i := 0; i < 3; i++ {
		if _, err := store.ClaimUndelivered(ctx, 5, 10); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		// MarkDelivered is a no-op (delivery truth is the idempotency marker).
		if err := store.MarkDelivered(ctx, "a"); err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
	}
	if got := store.All(); len(got) != 2 {
		t.Fatalf("append-only: claims/MarkDelivered must never delete a row, rows=%d want 2", len(got))
	}
}

// TestMemoryOutbox_PoisonCutoff proves a row stops being claimed once it reaches
// maxAttempts (the poison cutoff) and is parked, not deleted.
func TestMemoryOutbox_PoisonCutoff(t *testing.T) {
	store := persistence.NewMemoryOutboxStore(time.Nanosecond)
	tx := persistence.NewMemoryTxRunner(store)
	ctx := context.Background()
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "poison", EventType: "X"})

	const maxAttempts = 3
	for i := 0; i < maxAttempts; i++ {
		claimed, err := store.ClaimUndelivered(ctx, maxAttempts, 10)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if len(claimed) != 1 {
			t.Fatalf("attempt %d must still be claimable, got %d", i, len(claimed))
		}
	}
	after, err := store.ClaimUndelivered(ctx, maxAttempts, 10)
	if err != nil {
		t.Fatalf("post-cutoff claim: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("a row at maxAttempts must no longer be claimed, got %d", len(after))
	}
	if got := store.All(); len(got) != 1 {
		t.Fatalf("poison row must be parked (append-only), rows=%d", len(got))
	}
}

// TestMemoryOutbox_DropPartitionsBefore proves the OutboxRetention contract on the
// memory dev store: it forgets rows older than t (the dev model of a partition drop)
// while current-window rows survive.
func TestMemoryOutbox_DropPartitionsBefore(t *testing.T) {
	store := persistence.NewMemoryOutboxStore(time.Second)
	tx := persistence.NewMemoryTxRunner(store)
	ctx := context.Background()

	old := time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "old", EventType: "X", CreatedTime: old})
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "recent", EventType: "X", CreatedTime: recent})

	dropped, err := store.DropPartitionsBefore(ctx, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DropPartitionsBefore: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("retention must drop the one aged row, dropped=%d", dropped)
	}
	all := store.All()
	if len(all) != 1 || all[0].ID != "recent" {
		t.Fatalf("only the current-window row must survive, got %+v", all)
	}

	// The retention seam is satisfied.
	var _ persistence.OutboxRetention = store
}
