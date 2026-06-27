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

// openOutboxDB opens a shared-cache in-memory SQLite GORM db with the outbox
// table migrated. cache=shared keeps the in-memory db alive across connections so
// a committed write is visible to a fresh query.
func openOutboxDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&gormtx.OutboxRow{}); err != nil {
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

// TestGormOutbox_AppendOutsideTxErrors proves the F032 D-1 guard: Append without
// an enclosing Atomically returns ErrNoTransaction and writes nothing.
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

// TestGormOutbox_AppendRollsBackWithTx proves AC-1 (rollback side, the atomic
// enlist): an Append issued through the tx is discarded when the transaction
// rolls back — no orphan row on a separate connection.
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

// TestGormOutbox_LeaseLifecycle exercises Claim → MarkDelivered / Release.
func TestGormOutbox_LeaseLifecycle(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_lease")
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	store := gormtx.NewGormOutboxStore(db,
		gormtx.WithOutboxLeaseTTL(time.Minute),
		gormtx.WithOutboxNowForTest(clock.Now),
	)
	tx := gormtx.NewGormTxRunner(db)

	// Append two undelivered rows.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if aerr := store.Append(ctx, &persistence.OutboxRecord{ID: "e1", EventType: "X"}); aerr != nil {
			return aerr
		}
		return store.Append(ctx, &persistence.OutboxRecord{ID: "e2", EventType: "X"})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Claim both: attempts bumped, lease stamped.
	claimed, err := store.ClaimUndelivered(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected 2 claimed, got %d", len(claimed))
	}
	for _, r := range claimed {
		if r.Attempts != 1 {
			t.Fatalf("claim must bump attempts to 1, got %d for %s", r.Attempts, r.ID)
		}
	}

	// A second claim BEFORE the lease lapses returns nothing (leased rows hidden).
	again, err := store.ClaimUndelivered(context.Background(), 10)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("leased rows must be hidden from a competing claim, got %d", len(again))
	}

	// MarkDelivered e1 (terminal). Release e2 (drop lease for a prompt re-claim).
	if err := store.MarkDelivered(context.Background(), "e1"); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if err := store.Release(context.Background(), "e2"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Now a claim returns ONLY e2 (e1 is delivered/terminal; e2's lease was released).
	third, err := store.ClaimUndelivered(context.Background(), 10)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if len(third) != 1 || third[0].ID != "e2" {
		t.Fatalf("after MarkDelivered(e1)+Release(e2), claim must return only e2, got %+v", third)
	}
	if third[0].Attempts != 2 {
		t.Fatalf("re-claimed e2 must have attempts=2, got %d", third[0].Attempts)
	}
}

// TestGormOutbox_LeaseLapseAllowsReclaim proves a leased row re-claims once its
// lease lapses (the safety net behind at-least-once).
func TestGormOutbox_LeaseLapseAllowsReclaim(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_lapse")
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	store := gormtx.NewGormOutboxStore(db,
		gormtx.WithOutboxLeaseTTL(time.Minute),
		gormtx.WithOutboxNowForTest(clock.Now),
	)
	tx := gormtx.NewGormTxRunner(db)

	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		return store.Append(ctx, &persistence.OutboxRecord{ID: "e1", EventType: "X"})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if claimed, err := store.ClaimUndelivered(context.Background(), 10); err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: n=%d err=%v", len(claimed), err)
	}
	// Before the lease lapses: hidden.
	if again, _ := store.ClaimUndelivered(context.Background(), 10); len(again) != 0 {
		t.Fatalf("row must stay leased, got %d", len(again))
	}
	// Advance past the lease.
	clock.t = clock.t.Add(2 * time.Minute)
	reclaimed, err := store.ClaimUndelivered(context.Background(), 10)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Attempts != 2 {
		t.Fatalf("lapsed lease must re-claim with attempts=2, got %+v", reclaimed)
	}
}

// fakeClock is a settable clock for deterministic lease tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
