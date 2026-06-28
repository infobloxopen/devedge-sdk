package gormtx_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/cells"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// openBarrierDB opens a shared-cache in-memory SQLite db with the outbox + policy +
// allocator + sidecar-cursor tables migrated.
func openBarrierDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&gormtx.OutboxRow{}, &gormtx.TenantEventSeqRow{}, &gormtx.TenantEventPolicyRow{}, &gormtx.OutboxCursorRow{}, &gormtx.OutboxDeadLetterRow{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// TestOutboxEventBarrier_SetPolicy_ForwardOnly proves the policy epoch is forward-only
// and the stored policy/epoch round-trips.
func TestOutboxEventBarrier_SetPolicy_ForwardOnly(t *testing.T) {
	db := openBarrierDB(t, "barrier_setpolicy")
	b := gormtx.NewOutboxEventBarrier(db)
	ctx := context.Background()

	if err := b.SetPolicy(ctx, "t1", cells.PolicyPause, 5); err != nil {
		t.Fatalf("SetPolicy pause@5: %v", err)
	}
	pol, epoch, err := b.Policy(ctx, "t1")
	if err != nil || pol != cells.PolicyPause || epoch != 5 {
		t.Fatalf("expected (PAUSE,5), got (%v,%d) err=%v", pol, epoch, err)
	}
	// Forward to NORMAL@6.
	if err := b.SetPolicy(ctx, "t1", cells.PolicyNormal, 6); err != nil {
		t.Fatalf("SetPolicy normal@6: %v", err)
	}
	// Same epoch idempotent.
	if err := b.SetPolicy(ctx, "t1", cells.PolicyNormal, 6); err != nil {
		t.Fatalf("SetPolicy normal@6 idempotent: %v", err)
	}
	// Backward → ErrFenceRegression.
	if err := b.SetPolicy(ctx, "t1", cells.PolicyDrainQueue, 5); !errors.Is(err, cells.ErrFenceRegression) {
		t.Fatalf("backward epoch must be ErrFenceRegression, got %v", err)
	}
}

// TestOutboxEventBarrier_Drained proves Drained reflects pending outbox rows at
// event_epoch ≤ barrier relative to the relay's forward cursor.
func TestOutboxEventBarrier_Drained(t *testing.T) {
	db := openBarrierDB(t, "barrier_drained")
	store := gormtx.NewGormOutboxStore(db)
	tx := gormtx.NewGormTxRunner(db)
	cursors := gormtx.NewGormOutboxCursorStore(db)
	ctx := context.Background()

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Two events for t1 at epoch 1 (stamped explicitly), one for t2.
	if err := tx.Atomically(ctx, func(c context.Context) error {
		if e := store.Append(c, &persistence.OutboxRecord{ID: "e1", AccountID: "t1", EventType: "X", CreatedTime: base, EventEpoch: 1}); e != nil {
			return e
		}
		if e := store.Append(c, &persistence.OutboxRecord{ID: "e2", AccountID: "t1", EventType: "X", CreatedTime: base.Add(time.Minute), EventEpoch: 1}); e != nil {
			return e
		}
		return store.Append(c, &persistence.OutboxRecord{ID: "e3", AccountID: "t2", EventType: "X", CreatedTime: base, EventEpoch: 1})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	barrier := gormtx.NewOutboxEventBarrier(db, gormtx.WithBarrierCursors(cursors, "default"))

	// Relay at start of stream → t1 has 2 pending rows at epoch ≤ 1 → NOT drained.
	drained, err := barrier.Drained(ctx, "t1", 1)
	if err != nil {
		t.Fatalf("Drained: %v", err)
	}
	if drained {
		t.Fatal("t1 with two unpublished rows must NOT be drained")
	}

	// Advance the relay cursor past e1 only → still 1 pending (e2) → NOT drained.
	if err := cursors.SaveCursor(ctx, "default", persistence.OutboxCursor{CreatedTime: base, ID: "e1"}, 0); err != nil {
		t.Fatalf("save cursor e1: %v", err)
	}
	if drained, _ = barrier.Drained(ctx, "t1", 1); drained {
		t.Fatal("t1 with one remaining row (e2) must NOT be drained")
	}

	// Advance past e2 → t1 fully published → DRAINED.
	if err := cursors.SaveCursor(ctx, "default", persistence.OutboxCursor{CreatedTime: base.Add(time.Minute), ID: "e2"}, 0); err != nil {
		t.Fatalf("save cursor e2: %v", err)
	}
	if drained, _ = barrier.Drained(ctx, "t1", 1); !drained {
		t.Fatal("t1 with all rows at/behind the relay cursor must be drained")
	}

	// A tenant with no events is always drained.
	if drained, _ = barrier.Drained(ctx, "ghost", 1); !drained {
		t.Fatal("an event-free tenant must be drained")
	}

	// A higher-epoch row is NOT counted against a lower barrier: seed t3 at epoch 5
	// with a created_time AFTER the relay cursor (so it is genuinely unpublished), then
	// ask Drained at barrier epoch 1 → drained (the epoch-5 row is above the barrier).
	if err := tx.Atomically(ctx, func(c context.Context) error {
		return store.Append(c, &persistence.OutboxRecord{ID: "e9", AccountID: "t3", EventType: "X", CreatedTime: base.Add(time.Hour), EventEpoch: 5})
	}); err != nil {
		t.Fatalf("seed t3: %v", err)
	}
	if drained, _ = barrier.Drained(ctx, "t3", 1); !drained {
		t.Fatal("an above-barrier-epoch row must not block draining at a lower barrier")
	}
	if drained, _ = barrier.Drained(ctx, "t3", 5); drained {
		t.Fatal("the epoch-5 row IS pending at barrier 5 → not drained")
	}
}
