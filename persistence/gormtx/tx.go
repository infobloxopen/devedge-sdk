// Package gormtx provides the GORM-backed implementation of
// persistence.TxRunner. It is a sibling adapter to persistence/entrepo: the
// clean core (top-level persistence, authz, grpcauthz) never imports an ORM —
// only this adapter package depends on gorm.io/gorm.
//
// GormTxRunner is the GORM analogue of the generated EntTxRunner. It opens a
// single GORM transaction, stashes the transaction-scoped *gorm.DB on ctx via
// persistence.WithTx, and runs fn against it; the generated GORM repositories
// discover that handle (via persistence.TxFromContext) and bind their writes to
// the transaction for the duration of fn. The work commits when fn returns nil
// and rolls back when fn returns an error or panics.
package gormtx

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// GormTxRunner is the GORM-backed persistence.TxRunner for a *gorm.DB.
// Construct it with the same *gorm.DB the New<R>Repository constructors use; the
// generated repositories resolve their connection from the transaction it
// stashes on ctx, so writes issued inside Atomically participate in the
// transaction. The opaque ctx handle is the transaction-scoped *gorm.DB.
type GormTxRunner struct {
	db *gorm.DB
}

// NewGormTxRunner returns the GORM TxRunner over db.
func NewGormTxRunner(db *gorm.DB) *GormTxRunner {
	return &GormTxRunner{db: db}
}

// Atomically implements persistence.TxRunner.
//
// It uses manual Begin/Commit/Rollback rather than gorm's db.Transaction(fn)
// because the transaction-scoped *gorm.DB must be stashed on ctx (via
// persistence.WithTx) before fn runs, so the generated repositories can bind to
// it. A nested call joins a GORM transaction already on ctx (no second
// Begin/Commit). A panic rolls back and re-panics so it is never swallowed.
func (r *GormTxRunner) Atomically(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	// Nested: join a GORM transaction already on ctx (no second Begin/Commit).
	if h, ok := persistence.TxFromContext(ctx); ok {
		if _, ok := h.(*gorm.DB); ok {
			return fn(ctx)
		}
	}
	tx := r.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin tx: %w", tx.Error)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if ferr := fn(persistence.WithTx(ctx, tx)); ferr != nil {
		_ = tx.Rollback()
		return ferr
	}
	if cerr := tx.Commit().Error; cerr != nil {
		return fmt.Errorf("commit tx: %w", cerr)
	}
	return nil
}

// compile-time check.
var _ persistence.TxRunner = (*GormTxRunner)(nil)
