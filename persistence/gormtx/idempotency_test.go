package gormtx_test

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// effect is a tiny "aggregate write" row used to prove the idempotency marker
// commits/rolls back ATOMICALLY with the handler's own write.
type effect struct {
	ID string `gorm:"primaryKey"`
}

func openIdemDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&gormtx.IdemMarker{}, &effect{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// txConn resolves the ctx tx db so a test effect-write binds to the handler tx.
func txConn(ctx context.Context, db *gorm.DB) *gorm.DB {
	if h, ok := persistence.TxFromContext(ctx); ok {
		if tx, ok := h.(*gorm.DB); ok {
			return tx.WithContext(ctx)
		}
	}
	return db.WithContext(ctx)
}

// TestGormIdem_RecordThenDuplicateErrors proves the core exactly-once primitive:
// a first Record succeeds; a second Record of the SAME key returns
// events.ErrAlreadyApplied (the in-tx unique-marker conflict).
func TestGormIdem_RecordThenDuplicateErrors(t *testing.T) {
	db := openIdemDB(t, "gorm_idem_dup")
	store := gormtx.NewGormIdempotencyStore(db)
	tx := gormtx.NewGormTxRunner(db)

	const key = "evt-1\x00handler"

	// First Record commits.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		return store.Record(ctx, key)
	}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if seen, _ := store.Seen(context.Background(), key); !seen {
		t.Fatal("Seen must report the recorded key")
	}

	// Second Record of the same key must fail with ErrAlreadyApplied.
	err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		return store.Record(ctx, key)
	})
	if !errors.Is(err, events.ErrAlreadyApplied) {
		t.Fatalf("duplicate Record must return ErrAlreadyApplied, got %v", err)
	}
}

// TestGormIdem_MarkerIsTransactional proves the store is GENUINELY transactional
// (the gap the ent path leaves): the marker commits in the handler's OWN tx, so
// when the handler tx rolls back, the marker is gone too — it does not survive on
// a separate connection. This is what lets the exactly-once guard work: the marker
// and the effect are one atomic unit.
func TestGormIdem_MarkerIsTransactional(t *testing.T) {
	db := openIdemDB(t, "gorm_idem_atomic")
	store := gormtx.NewGormIdempotencyStore(db)
	tx := gormtx.NewGormTxRunner(db)

	const key = "evt-rollback\x00handler"
	boom := errors.New("handler failed after recording")

	// A handler that writes its effect AND records the marker, then fails.
	err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if cerr := txConn(ctx, db).Create(&effect{ID: "fx-1"}).Error; cerr != nil {
			return cerr
		}
		if rerr := store.Record(ctx, key); rerr != nil {
			return rerr
		}
		return boom // roll back BOTH the effect and the marker
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}

	// The marker rolled back with the effect: Seen is false, and a fresh Record
	// of the same key succeeds (it was never durably recorded).
	if seen, _ := store.Seen(context.Background(), key); seen {
		t.Fatal("a rolled-back Record must NOT leave a durable marker (not transactional)")
	}
	var fxCount int64
	db.WithContext(context.Background()).Model(&effect{}).Where("id = ?", "fx-1").Count(&fxCount)
	if fxCount != 0 {
		t.Fatalf("the effect must roll back with the marker, found %d", fxCount)
	}
	// Now a fresh Record of the key succeeds — proving nothing leaked.
	if rerr := tx.Atomically(context.Background(), func(ctx context.Context) error {
		return store.Record(ctx, key)
	}); rerr != nil {
		t.Fatalf("a fresh Record after a rolled-back attempt must succeed, got %v", rerr)
	}
}

// TestGormIdem_ExactlyOnceUnderDoubleApply proves AC-2 at the store level: two
// handler transactions both write the SAME effect-marker pair (a forced double
// delivery). Exactly one commits (effect + marker); the other collides on the
// unique marker, returns ErrAlreadyApplied, and its WHOLE tx — effect AND marker —
// rolls back. The effect is therefore applied exactly once.
func TestGormIdem_ExactlyOnceUnderDoubleApply(t *testing.T) {
	db := openIdemDB(t, "gorm_idem_exactly_once")
	store := gormtx.NewGormIdempotencyStore(db)
	tx := gormtx.NewGormTxRunner(db)

	const key = "evt-dup\x00handler"

	// applyOnce models one delivery of the handler: write the effect (idempotent
	// on the PK) AND record the marker in one tx. The marker is the gate.
	applyOnce := func(effectID string) error {
		return tx.Atomically(context.Background(), func(ctx context.Context) error {
			if rerr := store.Record(ctx, key); rerr != nil {
				return rerr // ErrAlreadyApplied here rolls the effect back too
			}
			return txConn(ctx, db).Create(&effect{ID: effectID}).Error
		})
	}

	// First delivery commits.
	if err := applyOnce("fx-A"); err != nil {
		t.Fatalf("first delivery must commit, got %v", err)
	}
	// Second delivery of the SAME event must be a no-op (ErrAlreadyApplied) and
	// must NOT write its effect.
	if err := applyOnce("fx-B"); !errors.Is(err, events.ErrAlreadyApplied) {
		t.Fatalf("second delivery must return ErrAlreadyApplied, got %v", err)
	}

	// Exactly one effect row exists, and it is the first delivery's.
	var n int64
	db.WithContext(context.Background()).Model(&effect{}).Count(&n)
	if n != 1 {
		t.Fatalf("exactly-once: there must be exactly one effect row, found %d", n)
	}
	var got effect
	if err := db.WithContext(context.Background()).First(&got).Error; err != nil {
		t.Fatalf("read effect: %v", err)
	}
	if got.ID != "fx-A" {
		t.Fatalf("the surviving effect must be the first delivery's (fx-A), got %q", got.ID)
	}
}
