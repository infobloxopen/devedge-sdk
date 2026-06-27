package gormtx_test

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// widget is a tiny GORM model used to prove the GormTxRunner commits/rolls back.
type widget struct {
	ID   string `gorm:"primaryKey"`
	Name string
}

// widgetRepo is a minimal tx-aware repository: its conn(ctx) resolver mirrors
// exactly what protoc-gen-storage now emits, so the test proves the runner +
// resolver contract end-to-end (a write inside Atomically binds to the tx).
type widgetRepo struct{ db *gorm.DB }

func (r *widgetRepo) conn(ctx context.Context) *gorm.DB {
	if h, ok := persistence.TxFromContext(ctx); ok {
		if tx, ok := h.(*gorm.DB); ok {
			return tx.WithContext(ctx)
		}
	}
	return r.db.WithContext(ctx)
}

func (r *widgetRepo) create(ctx context.Context, w *widget) error {
	return r.conn(ctx).Create(w).Error
}

func (r *widgetRepo) get(ctx context.Context, id string) (*widget, error) {
	var w widget
	if err := r.conn(ctx).Where("id = ?", id).First(&w).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	return &w, nil
}

func openWidgetDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&widget{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// TestGormAtomically_RollbackOnError: a write issued through the tx-aware repo
// inside Atomically is discarded when fn returns an error.
func TestGormAtomically_RollbackOnError(t *testing.T) {
	db := openWidgetDB(t, "gormtx_rollback")
	repo := &widgetRepo{db: db}
	tx := gormtx.NewGormTxRunner(db)

	wantErr := errors.New("forced mid-fn failure")
	err := tx.Atomically(context.Background(), func(txCtx context.Context) error {
		if cerr := repo.create(txCtx, &widget{ID: "w-1", Name: "first"}); cerr != nil {
			return cerr
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Atomically: want forced error, got %v", err)
	}

	if _, gerr := repo.get(context.Background(), "w-1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("w-1 must be rolled back, got %v", gerr)
	}
}

// TestGormAtomically_CommitOnSuccess: the write is committed and visible through
// the non-tx repo when fn returns nil.
func TestGormAtomically_CommitOnSuccess(t *testing.T) {
	db := openWidgetDB(t, "gormtx_commit")
	repo := &widgetRepo{db: db}
	tx := gormtx.NewGormTxRunner(db)

	if err := tx.Atomically(context.Background(), func(txCtx context.Context) error {
		return repo.create(txCtx, &widget{ID: "w-1", Name: "first"})
	}); err != nil {
		t.Fatalf("Atomically: %v", err)
	}

	got, err := repo.get(context.Background(), "w-1")
	if err != nil {
		t.Fatalf("w-1 must be committed: %v", err)
	}
	if got.Name != "first" {
		t.Fatalf("committed widget wrong: %+v", got)
	}
}

// TestGormAtomically_TxBoundReadsSeeUncommitted: a tx-bound read sees the
// transaction's own uncommitted write, and it is discarded on rollback.
func TestGormAtomically_TxBoundReadsSeeUncommitted(t *testing.T) {
	db := openWidgetDB(t, "gormtx_visibility")
	repo := &widgetRepo{db: db}
	tx := gormtx.NewGormTxRunner(db)

	boom := errors.New("rollback")
	err := tx.Atomically(context.Background(), func(txCtx context.Context) error {
		if cerr := repo.create(txCtx, &widget{ID: "w-1", Name: "first"}); cerr != nil {
			return cerr
		}
		if _, gerr := repo.get(txCtx, "w-1"); gerr != nil {
			return errors.New("tx-bound read should see the uncommitted write: " + gerr.Error())
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Atomically: want rollback error, got %v", err)
	}

	if _, gerr := repo.get(context.Background(), "w-1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("rolled-back write must be invisible, got %v", gerr)
	}
}

// TestGormAtomically_NestedJoinsOuter: a nested Atomically joins the outer
// transaction (no second commit), so a rollback at the outer level discards the
// inner write too.
func TestGormAtomically_NestedJoinsOuter(t *testing.T) {
	db := openWidgetDB(t, "gormtx_nested")
	repo := &widgetRepo{db: db}
	tx := gormtx.NewGormTxRunner(db)

	boom := errors.New("rollback")
	err := tx.Atomically(context.Background(), func(outerCtx context.Context) error {
		if ierr := tx.Atomically(outerCtx, func(innerCtx context.Context) error {
			return repo.create(innerCtx, &widget{ID: "w-1", Name: "nested"})
		}); ierr != nil {
			return ierr
		}
		// The inner Atomically must NOT have committed independently.
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Atomically: want rollback error, got %v", err)
	}
	if _, gerr := repo.get(context.Background(), "w-1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("nested write must roll back with the outer tx, got %v", gerr)
	}
}

// TestGormAtomically_ForeignHandleOpensOwnTx: when ctx already carries a handle
// from a DIFFERENT backend (e.g. an *ent.Tx, modelled here by a non-*gorm.DB
// sentinel), GormTxRunner must NOT mis-join it. It opens its OWN gorm transaction
// and commits/rolls back that, leaving the foreign handle untouched. This guards
// the type-narrowing in the nested-join branch (h.(*gorm.DB)).
func TestGormAtomically_ForeignHandleOpensOwnTx(t *testing.T) {
	db := openWidgetDB(t, "gormtx_foreign")
	repo := &widgetRepo{db: db}
	tx := gormtx.NewGormTxRunner(db)

	// A foreign (non-*gorm.DB) tx handle, as another backend's runner would stash.
	type entTxLike struct{ name string }
	foreignCtx := persistence.WithTx(context.Background(), &entTxLike{name: "ent"})

	// Commit path: GormTxRunner must open its own gorm tx and commit it, even
	// though ctx carries the foreign handle.
	if err := tx.Atomically(foreignCtx, func(txCtx context.Context) error {
		// Inside fn, the ctx handle must now be the gorm tx (WithTx overwrote the
		// foreign one for the duration of fn), so the tx-aware repo binds to it.
		if h, ok := persistence.TxFromContext(txCtx); !ok {
			t.Fatalf("expected a tx handle on ctx inside fn")
		} else if _, ok := h.(*gorm.DB); !ok {
			t.Fatalf("expected the ctx handle inside fn to be the gorm tx, got %T", h)
		}
		return repo.create(txCtx, &widget{ID: "w-own", Name: "own-tx"})
	}); err != nil {
		t.Fatalf("Atomically (foreign handle, commit): %v", err)
	}
	if got, err := repo.get(context.Background(), "w-own"); err != nil {
		t.Fatalf("write under self-opened tx must commit: %v", err)
	} else if got.Name != "own-tx" {
		t.Fatalf("committed widget wrong: %+v", got)
	}

	// Rollback path: a self-opened tx must still roll back on error (proving it
	// did NOT silently run on the foreign handle / autocommit).
	boom := errors.New("rollback")
	err := tx.Atomically(foreignCtx, func(txCtx context.Context) error {
		if cerr := repo.create(txCtx, &widget{ID: "w-own-rb", Name: "discard"}); cerr != nil {
			return cerr
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Atomically (foreign handle, rollback): want boom, got %v", err)
	}
	if _, gerr := repo.get(context.Background(), "w-own-rb"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("self-opened tx must roll back, got %v", gerr)
	}
}

// TestGormAtomically_PanicRollsBackAndRepanics: a panic inside fn rolls back and
// re-panics (it is never swallowed).
func TestGormAtomically_PanicRollsBackAndRepanics(t *testing.T) {
	db := openWidgetDB(t, "gormtx_panic")
	repo := &widgetRepo{db: db}
	tx := gormtx.NewGormTxRunner(db)

	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = tx.Atomically(context.Background(), func(txCtx context.Context) error {
			_ = repo.create(txCtx, &widget{ID: "w-1", Name: "boom"})
			panic("kaboom")
		})
	}()

	if _, gerr := repo.get(context.Background(), "w-1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("panicked write must be rolled back, got %v", gerr)
	}
}
