package gormtx_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// openOutboxDB opens a shared-cache in-memory SQLite GORM db with the write-only outbox
// table plus the dispatcher sidecar tables migrated. cache=shared keeps the in-memory
// db alive across connections so a committed write is visible to a fresh query.
func openOutboxDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&gormtx.OutboxRow{}, &gormtx.OutboxCursorRow{}, &gormtx.OutboxDeadLetterRow{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// countOutbox returns how many rows match id on a fresh (non-tx) connection.
func countOutbox(t *testing.T, db *gorm.DB, id string) int64 {
	t.Helper()
	var n int64
	if err := db.WithContext(context.Background()).Model(&gormtx.OutboxRow{}).Where("id = ?", id).Count(&n).Error; err != nil {
		t.Fatalf("count outbox %q: %v", id, err)
	}
	return n
}

// TestGormOutbox_AppendOutsideTxErrors proves the F032 D-1 guard: Append without an
// enclosing Atomically returns ErrNoTransaction and writes nothing.
func TestGormOutbox_AppendOutsideTxErrors(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_notx")
	store := gormtx.NewGormOutboxStore(db)

	err := store.Append(context.Background(), &persistence.OutboxRecord{ID: "no-tx", EventType: "X"})
	if !errors.Is(err, persistence.ErrNoTransaction) {
		t.Fatalf("Append outside a tx must return ErrNoTransaction, got %v", err)
	}
	if n := countOutbox(t, db, "no-tx"); n != 0 {
		t.Fatalf("a refused Append must write no row, found %d", n)
	}
}

// TestGormOutbox_AppendCommitsWithTx proves AC-1 (commit side): an Append issued
// through the tx commits with the transaction and is then visible.
func TestGormOutbox_AppendCommitsWithTx(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_commit")
	store := gormtx.NewGormOutboxStore(db)
	tx := gormtx.NewGormTxRunner(db)

	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		return store.Append(ctx, &persistence.OutboxRecord{ID: "evt-commit", EventType: "X", AggregateID: "a1"})
	}); err != nil {
		t.Fatalf("Atomically: %v", err)
	}
	if n := countOutbox(t, db, "evt-commit"); n != 1 {
		t.Fatalf("a committed Append must leave exactly one row, found %d", n)
	}
}

// TestGormOutbox_AppendRollsBackWithTx proves AC-1 (rollback side, the atomic enlist):
// an Append issued through the tx is discarded when the transaction rolls back — no
// orphan row on a separate connection.
func TestGormOutbox_AppendRollsBackWithTx(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_rollback")
	store := gormtx.NewGormOutboxStore(db)
	tx := gormtx.NewGormTxRunner(db)

	boom := errors.New("boom")
	err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if aerr := store.Append(ctx, &persistence.OutboxRecord{ID: "evt-rollback", EventType: "X", AggregateID: "a1"}); aerr != nil {
			return aerr
		}
		return boom // force rollback AFTER the append
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	if n := countOutbox(t, db, "evt-rollback"); n != 0 {
		t.Fatalf("rollback must discard the outbox row (atomic enlist), found %d", n)
	}
}

// TestGormOutbox_WriteOnly_ReadForwardCursor exercises the F033 write-only forward
// scan: ReadAfter returns rows strictly after the cursor in (created_time, id) order
// and NEVER mutates a row. The dispatcher's progress lives in the sidecar, not the
// outbox, so reading the same cursor twice returns the same rows and the row count is
// unchanged.
func TestGormOutbox_WriteOnly_ReadForwardCursor(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_cursor")
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	store := gormtx.NewGormOutboxStore(db, gormtx.WithOutboxNowForTest(clock.Now))
	tx := gormtx.NewGormTxRunner(db)

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if aerr := store.Append(ctx, &persistence.OutboxRecord{ID: "e1", EventType: "X", CreatedTime: base}); aerr != nil {
			return aerr
		}
		if aerr := store.Append(ctx, &persistence.OutboxRecord{ID: "e2", EventType: "X", CreatedTime: base}); aerr != nil {
			return aerr // shares e1's created_time; id breaks the tie
		}
		return store.Append(ctx, &persistence.OutboxRecord{ID: "e3", EventType: "X", CreatedTime: base.Add(time.Minute)})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// From the start, limit 2: the two oldest in (created_time, id) order.
	got, err := store.ReadAfter(context.Background(), persistence.OutboxCursor{}, 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 || got[0].ID != "e1" || got[1].ID != "e2" {
		t.Fatalf("forward scan must return [e1 e2], got %+v", got)
	}
	// Reading the same cursor again is identical (non-mutating).
	again, _ := store.ReadAfter(context.Background(), persistence.OutboxCursor{}, 2)
	if len(again) != 2 || again[0].ID != "e1" || again[1].ID != "e2" {
		t.Fatalf("ReadAfter must be non-mutating and repeatable, got %+v", again)
	}
	// Advance the cursor past e2; next read returns only e3.
	cursor := persistence.OutboxCursor{CreatedTime: got[1].CreatedTime, ID: got[1].ID}
	next, err := store.ReadAfter(context.Background(), cursor, 10)
	if err != nil {
		t.Fatalf("read after cursor: %v", err)
	}
	if len(next) != 1 || next[0].ID != "e3" {
		t.Fatalf("read after e2 must return only e3, got %+v", next)
	}

	// Write-only: the three rows are intact (reads never deleted/added).
	var total int64
	if err := db.WithContext(context.Background()).Model(&gormtx.OutboxRow{}).Count(&total).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 3 {
		t.Fatalf("write-only: ReadAfter must never mutate the table, count=%d want 3", total)
	}
}

// TestGormOutboxCursorStore_LoadSaveDeadLetter proves the sidecar cursor store on GORM:
// a never-saved cursor is the zero position; SaveCursor upserts the position +
// head-failure count; DeadLetter parks a poison event. None of this touches the
// write-only outbox.
func TestGormOutboxCursorStore_LoadSaveDeadLetter(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_sidecar")
	cur := gormtx.NewGormOutboxCursorStore(db)
	ctx := context.Background()

	pos, fails, err := cur.LoadCursor(ctx, "default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !pos.IsZero() || fails != 0 {
		t.Fatalf("a never-saved cursor must be zero/0, got %+v fails=%d", pos, fails)
	}

	want := persistence.OutboxCursor{CreatedTime: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), ID: "e7"}
	if err := cur.SaveCursor(ctx, "default", want, 2); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Re-save (upsert) to a new position to prove it updates, not duplicates.
	want2 := persistence.OutboxCursor{CreatedTime: want.CreatedTime.Add(time.Hour), ID: "e9"}
	if err := cur.SaveCursor(ctx, "default", want2, 0); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, fails, err := cur.LoadCursor(ctx, "default")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.CreatedTime.Equal(want2.CreatedTime) || got.ID != want2.ID || fails != 0 {
		t.Fatalf("cursor upsert must reflect the latest save, got %+v fails=%d", got, fails)
	}
	var cursorRows int64
	db.WithContext(ctx).Model(&gormtx.OutboxCursorRow{}).Count(&cursorRows)
	if cursorRows != 1 {
		t.Fatalf("SaveCursor must upsert (one row per cursor name), got %d rows", cursorRows)
	}

	if err := cur.DeadLetter(ctx, "default", &persistence.OutboxRecord{ID: "poison", EventType: "T", CreatedTime: want.CreatedTime}, "boom"); err != nil {
		t.Fatalf("dead-letter: %v", err)
	}
	var dead int64
	db.WithContext(ctx).Model(&gormtx.OutboxDeadLetterRow{}).Where("event_id = ?", "poison").Count(&dead)
	if dead != 1 {
		t.Fatalf("dead-letter must park the poison event, got %d", dead)
	}
}

// TestGormOutbox_DropPartitionsBefore_SQLite proves the dev-backend retention model: on
// the non-partitioned SQLite backend, DropPartitionsBefore forgets rows older than t
// (the windowed model of a partition drop) while current-window rows survive. It is the
// OutboxRetention contract for the dev backend (the real partition DDL is exercised on
// PG/MySQL in the iam fixture).
func TestGormOutbox_DropPartitionsBefore_SQLite(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_retention")
	store := gormtx.NewGormOutboxStore(db)
	tx := gormtx.NewGormTxRunner(db)

	old := time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if aerr := store.Append(ctx, &persistence.OutboxRecord{ID: "old", EventType: "X", CreatedTime: old}); aerr != nil {
			return aerr
		}
		return store.Append(ctx, &persistence.OutboxRecord{ID: "recent", EventType: "X", CreatedTime: recent})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cutoff := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	dropped, err := store.DropPartitionsBefore(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("DropPartitionsBefore: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("retention must drop the one aged row, dropped=%d", dropped)
	}
	if n := countOutbox(t, db, "old"); n != 0 {
		t.Fatalf("the aged row must be gone, found %d", n)
	}
	if n := countOutbox(t, db, "recent"); n != 1 {
		t.Fatalf("the current-window row must survive, found %d", n)
	}
}

// fakeClock is a settable clock for deterministic created-time tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
