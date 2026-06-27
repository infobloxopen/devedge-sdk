// idempotency.go — the GORM/SQL-backed events.IdempotencyStore (F032). This is
// the genuinely transactional, exactly-once idempotency store the ent path lacks:
// the ent fixture relies on the in-memory MemoryIdempotencyStore, whose marker
// does NOT commit in the handler's ent transaction. Here Record inserts a marker
// row THROUGH the handler's GORM transaction (via persistence.TxFromContext), so
// the marker commits ATOMICALLY with the handler's aggregate write — exactly the
// contract events.IdempotencyStore documents. A concurrent (or lapsed-lease)
// double-delivery races to insert the same primary key; exactly one transaction
// commits (effect + marker) and the other gets a unique-key conflict, which
// Record maps to events.ErrAlreadyApplied so the duplicate effect rolls back
// (F032 AC-2).
//
// gormtx may import events without a cycle: events depends only on persistence
// (the OutboxStore seam + the F030 tx helpers), never on gormtx.
package gormtx

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// IdemMarker is the row stored per applied (event, handler) key. Keyed by the
// idempotency key so a duplicate Record is a primary-key conflict — the in-tx
// uniqueness that serializes a concurrent double-apply.
type IdemMarker struct {
	Key string `gorm:"primaryKey;type:varchar(255)"`
}

// TableName pins the table to "idempotency_markers".
func (IdemMarker) TableName() string { return "idempotency_markers" }

// GormIdempotencyStore is the GORM-backed events.IdempotencyStore. Construct it
// with the same *gorm.DB the handler's GormTxRunner uses, so Record's marker
// insert binds to the handler's transaction and commits with the handler's
// aggregate write.
type GormIdempotencyStore struct {
	db *gorm.DB
}

// NewGormIdempotencyStore returns a GORM-backed IdempotencyStore over db.
func NewGormIdempotencyStore(db *gorm.DB) *GormIdempotencyStore {
	return &GormIdempotencyStore{db: db}
}

// conn resolves the transaction-scoped *gorm.DB when ctx carries one (so Record
// commits with the handler's tx); otherwise the base db (the Seen fast-path).
func (s *GormIdempotencyStore) conn(ctx context.Context) *gorm.DB {
	if h, ok := persistence.TxFromContext(ctx); ok {
		if tx, ok := h.(*gorm.DB); ok {
			return tx.WithContext(ctx)
		}
	}
	return s.db.WithContext(ctx)
}

// Seen implements events.IdempotencyStore: a fast-path pre-check on the base db
// that reports whether key has already been recorded. Correctness does NOT depend
// on Seen — the in-tx Record below is the real exactly-once guard.
func (s *GormIdempotencyStore) Seen(ctx context.Context, key string) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&IdemMarker{}).
		Where("key = ?", key).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("idempotency seen %q: %w", key, err)
	}
	return count > 0, nil
}

// Record implements events.IdempotencyStore: insert the marker INSIDE the
// handler's transaction (resolved from ctx), so the marker commits atomically
// with the handler's aggregate write. A duplicate key is a unique/PK constraint
// violation, surfaced as events.ErrAlreadyApplied so the dispatcher rolls the
// duplicate effect back — the side effect runs exactly once even under a
// double-delivery (F032 AC-2).
func (s *GormIdempotencyStore) Record(ctx context.Context, key string) error {
	if err := s.conn(ctx).Create(&IdemMarker{Key: key}).Error; err != nil {
		if ce := persistence.ConstraintError(err); errors.Is(ce, persistence.ErrConflict) {
			return events.ErrAlreadyApplied
		}
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return events.ErrAlreadyApplied
		}
		return fmt.Errorf("idempotency record %q: %w", key, err)
	}
	return nil
}

// compile-time check.
var _ events.IdempotencyStore = (*GormIdempotencyStore)(nil)
