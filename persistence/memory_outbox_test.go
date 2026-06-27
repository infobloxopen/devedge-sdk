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

// TestMemoryOutbox_WriteOnly_ReadNeverMutates proves the F033 write-only invariant on
// the memory dev store: ReadAfter is a non-mutating forward scan — reading the same
// cursor repeatedly returns the same rows and never deletes, marks, or otherwise
// changes a row. The row count only grows until a partition drop.
func TestMemoryOutbox_WriteOnly_ReadNeverMutates(t *testing.T) {
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(store)
	ctx := context.Background()

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "a", EventType: "X", CreatedTime: base})
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "b", EventType: "X", CreatedTime: base.Add(time.Second)})

	// Read from the start three times; each read is identical and mutates nothing.
	for i := 0; i < 3; i++ {
		got, err := store.ReadAfter(ctx, persistence.OutboxCursor{}, 10)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
			t.Fatalf("read %d must return [a b] in order, got %+v", i, got)
		}
	}
	// Write-only: reads never deleted or added a row — the count is unchanged.
	if all := store.All(); len(all) != 2 {
		t.Fatalf("write-only: ReadAfter must never delete/add a row, rows=%d want 2", len(all))
	}
}

// TestMemoryOutbox_ForwardCursorOrder proves ReadAfter returns rows strictly after the
// cursor in (created_time, id) order — the forward-cursor scan the dispatcher consumes.
func TestMemoryOutbox_ForwardCursorOrder(t *testing.T) {
	store := persistence.NewMemoryOutboxStore()
	tx := persistence.NewMemoryTxRunner(store)
	ctx := context.Background()

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Two events share a created_time: id breaks the tie so the scan is total.
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "e1", EventType: "X", CreatedTime: base})
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "e2", EventType: "X", CreatedTime: base})
	appendTx(t, tx, store, &persistence.OutboxRecord{ID: "e3", EventType: "X", CreatedTime: base.Add(time.Minute)})

	// From the start, limit 2: the two oldest in order.
	got, err := store.ReadAfter(ctx, persistence.OutboxCursor{}, 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 || got[0].ID != "e1" || got[1].ID != "e2" {
		t.Fatalf("forward scan must return [e1 e2], got %+v", got)
	}
	// Advance the cursor past e2; the next read returns only e3.
	cursor := persistence.OutboxCursor{CreatedTime: got[1].CreatedTime, ID: got[1].ID}
	next, err := store.ReadAfter(ctx, cursor, 10)
	if err != nil {
		t.Fatalf("read after cursor: %v", err)
	}
	if len(next) != 1 || next[0].ID != "e3" {
		t.Fatalf("read after e2 must return only e3, got %+v", next)
	}
	// At the end of the stream: nothing more.
	end := persistence.OutboxCursor{CreatedTime: next[0].CreatedTime, ID: next[0].ID}
	if tail, _ := store.ReadAfter(ctx, end, 10); len(tail) != 0 {
		t.Fatalf("cursor at the head must read nothing, got %+v", tail)
	}
}

// TestMemoryOutbox_DropPartitionsBefore proves the OutboxRetention contract on the
// memory dev store: it forgets rows older than t (the dev model of a partition drop)
// while current-window rows survive.
func TestMemoryOutbox_DropPartitionsBefore(t *testing.T) {
	store := persistence.NewMemoryOutboxStore()
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

// TestMemoryOutboxCursorStore_LoadSaveDeadLetter proves the sidecar cursor store: a
// never-saved cursor is the zero position; SaveCursor round-trips the position +
// head-failure count; DeadLetter parks a poison event. None of this touches the
// write-only outbox (it is an independent sidecar).
func TestMemoryOutboxCursorStore_LoadSaveDeadLetter(t *testing.T) {
	cur := persistence.NewMemoryOutboxCursorStore()
	ctx := context.Background()

	// A fresh cursor is start-of-stream with zero head failures.
	pos, fails, err := cur.LoadCursor(ctx, "default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !pos.IsZero() || fails != 0 {
		t.Fatalf("a never-saved cursor must be zero/0, got %+v fails=%d", pos, fails)
	}

	// Save a position with a head-failure count, then read it back.
	want := persistence.OutboxCursor{CreatedTime: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), ID: "e7"}
	if err := cur.SaveCursor(ctx, "default", want, 2); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, fails, err := cur.LoadCursor(ctx, "default")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.CreatedTime.Equal(want.CreatedTime) || got.ID != want.ID || fails != 2 {
		t.Fatalf("cursor must round-trip, got %+v fails=%d", got, fails)
	}

	// Dead-letter a poison event into the sidecar.
	if err := cur.DeadLetter(ctx, "default", &persistence.OutboxRecord{ID: "poison", EventType: "T", CreatedTime: want.CreatedTime}, "boom"); err != nil {
		t.Fatalf("dead-letter: %v", err)
	}
	dead := cur.DeadLettered()
	if len(dead) != 1 || dead[0].EventID != "poison" || dead[0].Reason != "boom" {
		t.Fatalf("dead-letter must park the poison event, got %+v", dead)
	}
}
