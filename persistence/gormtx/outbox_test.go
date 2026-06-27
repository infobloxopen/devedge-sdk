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

// TestGormOutbox_LeaseLifecycle exercises Claim → Release under the F033 append-only
// model (MarkDelivered is a no-op; delivery truth is the idempotency marker, so the
// store never marks a row terminal). A claimed row is hidden by its lease; a released
// row is immediately re-claimable; attempts increment on every claim.
func TestGormOutbox_LeaseLifecycle(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_lease")
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	store := gormtx.NewGormOutboxStore(db,
		gormtx.WithOutboxLeaseTTL(time.Minute),
		gormtx.WithOutboxNowForTest(clock.Now),
	)
	tx := gormtx.NewGormTxRunner(db)

	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if aerr := store.Append(ctx, &persistence.OutboxRecord{ID: "e1", EventType: "X"}); aerr != nil {
			return aerr
		}
		return store.Append(ctx, &persistence.OutboxRecord{ID: "e2", EventType: "X"})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Claim both: attempts bumped, lease stamped.
	claimed, err := store.ClaimUndelivered(context.Background(), 5, 10)
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
	again, err := store.ClaimUndelivered(context.Background(), 5, 10)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("leased rows must be hidden from a competing claim, got %d", len(again))
	}

	// MarkDelivered is a NO-OP under the append-only model: it must NOT make e1
	// terminal (no row-state write). Release e2 to drop its lease for a prompt re-claim.
	if err := store.MarkDelivered(context.Background(), "e1"); err != nil {
		t.Fatalf("mark delivered (no-op): %v", err)
	}
	if err := store.Release(context.Background(), "e2"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// e1 stays leased (MarkDelivered did nothing); only e2 (released) re-claims.
	third, err := store.ClaimUndelivered(context.Background(), 5, 10)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if len(third) != 1 || third[0].ID != "e2" {
		t.Fatalf("after Release(e2), claim must return only e2 (e1 still leased, MarkDelivered is a no-op), got %+v", third)
	}
	if third[0].Attempts != 2 {
		t.Fatalf("re-claimed e2 must have attempts=2, got %d", third[0].Attempts)
	}

	// AC-1 (append-only): MarkDelivered and the claims above NEVER deleted a row. The
	// table still holds both rows.
	var total int64
	if err := db.WithContext(context.Background()).Model(&gormtx.OutboxRow{}).Count(&total).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 2 {
		t.Fatalf("append-only: the dispatch path must never delete a row, count=%d want 2", total)
	}
}

// TestGormOutbox_PoisonCutoff proves AC-3's poison half: a row stops being claimed
// once its attempts reach maxAttempts, so a permanently failing event does not loop
// forever (and the append-only row is never deleted — it is parked for retention).
func TestGormOutbox_PoisonCutoff(t *testing.T) {
	db := openOutboxDB(t, "gorm_outbox_poison")
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	store := gormtx.NewGormOutboxStore(db,
		gormtx.WithOutboxLeaseTTL(time.Minute),
		gormtx.WithOutboxNowForTest(clock.Now),
	)
	tx := gormtx.NewGormTxRunner(db)
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		return store.Append(ctx, &persistence.OutboxRecord{ID: "poison", EventType: "X"})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const maxAttempts = 3
	for i := 0; i < maxAttempts; i++ {
		claimed, err := store.ClaimUndelivered(context.Background(), maxAttempts, 10)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if len(claimed) != 1 {
			t.Fatalf("attempt %d must still be claimable (attempts=%d < max=%d), got %d claimed", i, i, maxAttempts, len(claimed))
		}
		// Simulate a failed delivery: release the lease so the next claim is prompt.
		if err := store.Release(context.Background(), "poison"); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
	// Now attempts == maxAttempts: the row is poison and no longer claimed.
	after, err := store.ClaimUndelivered(context.Background(), maxAttempts, 10)
	if err != nil {
		t.Fatalf("post-cutoff claim: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("a row at maxAttempts must no longer be claimed (poison cutoff), got %d", len(after))
	}
	// Append-only: the poison row is still present (parked for retention), not deleted.
	var total int64
	db.WithContext(context.Background()).Model(&gormtx.OutboxRow{}).Count(&total)
	if total != 1 {
		t.Fatalf("poison row must be parked (append-only), not deleted; count=%d", total)
	}
}

// TestGormOutbox_DropPartitionsBefore_SQLite proves the dev-backend retention model:
// on the non-partitioned SQLite backend, DropPartitionsBefore forgets rows older than
// t (the windowed model of a partition drop) while current-window rows survive. It is
// the OutboxRetention contract for the dev backend (the real partition DDL is exercised
// on PG in the iam fixture).
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

	if claimed, err := store.ClaimUndelivered(context.Background(), 5, 10); err != nil || len(claimed) != 1 {
		t.Fatalf("first claim: n=%d err=%v", len(claimed), err)
	}
	// Before the lease lapses: hidden.
	if again, _ := store.ClaimUndelivered(context.Background(), 5, 10); len(again) != 0 {
		t.Fatalf("row must stay leased, got %d", len(again))
	}
	// Advance past the lease.
	clock.t = clock.t.Add(2 * time.Minute)
	reclaimed, err := store.ClaimUndelivered(context.Background(), 5, 10)
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
