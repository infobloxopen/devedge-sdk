// dedupstore.go — the GORM/SQL-backed durable, exactly-once request-idempotency
// store (WS-043 / F048). It generalizes the marker-only GormIdempotencyStore
// (which stores a bare key for event dedup) into a tenant-scoped, per-operation
// store that also persists the RESPONSE for verbatim replay.
//
// Claim inserts an in_progress row THROUGH the handler's transaction
// (persistence.TxFromContext); Complete transitions it to completed with the
// serialized response in the SAME transaction — so the claim, the domain effect,
// and the stored response commit as one unit (exactly-once). Lookup is a
// non-transactional fast path a retry hits to replay a completed response without
// running any domain transaction. The primary key (account_id, method,
// request_id) both scopes the key per tenant+operation (no cross-tenant/cross-
// method collision) and gives the in-tx uniqueness that serializes concurrent
// duplicates. account_id is a first-class column so WS-029 row-level security
// covers the table with the same tenant GUC set at transaction Begin.
package gormtx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// IdempotencyKeyRow is the durable idempotency record. The composite primary key
// is the tenant-scoped, per-operation identity; expires_at bounds retention and is
// indexed for GC; response_type + response hold the marshaled proto response for
// replay; fingerprint optionally guards against a key reused with a different body.
type IdempotencyKeyRow struct {
	AccountID    string    `gorm:"primaryKey;column:account_id;type:varchar(255)"`
	Method       string    `gorm:"primaryKey;column:method;type:varchar(255)"`
	RequestID    string    `gorm:"primaryKey;column:request_id;type:varchar(255)"`
	Status       string    `gorm:"column:status;type:varchar(16);not null"`
	ResponseType string    `gorm:"column:response_type;type:varchar(255)"`
	Response     []byte    `gorm:"column:response;type:bytea"`
	Fingerprint  string    `gorm:"column:fingerprint;type:varchar(64)"`
	CreatedAt    time.Time `gorm:"column:created_at;not null"`
	ExpiresAt    time.Time `gorm:"column:expires_at;not null;index"`
}

// TableName pins the table to "idempotency_keys".
func (IdempotencyKeyRow) TableName() string { return idempotencyKeysBaseTable }

// idempotencyKeysBaseTable is the unqualified durable-dedup table name.
const idempotencyKeysBaseTable = "idempotency_keys"

// GormDurableDedupStore is the GORM-backed durable idempotency store. Construct it
// with the same *gorm.DB the handler's GormTxRunner uses so Claim/Complete bind to
// the handler's transaction. It satisfies middleware.DurableIdempotencyStore.
type GormDurableDedupStore struct {
	db    *gorm.DB
	table string
	now   func() time.Time
}

// DurableDedupOption configures a GormDurableDedupStore.
type DurableDedupOption func(*GormDurableDedupStore)

// WithDurableDedupNamespace qualifies the table per a module's
// persistence.DatabaseNamespace (WS-012), so co-resident modules get isolated
// idempotency_keys tables. The zero namespace leaves the bare name.
func WithDurableDedupNamespace(ns persistence.DatabaseNamespace) DurableDedupOption {
	return func(s *GormDurableDedupStore) { s.table = ns.QualifyTable(idempotencyKeysBaseTable) }
}

// WithDurableDedupClock overrides the store's clock (for TTL/GC tests).
func WithDurableDedupClock(now func() time.Time) DurableDedupOption {
	return func(s *GormDurableDedupStore) { s.now = now }
}

// NewGormDurableDedupStore returns a GORM-backed durable idempotency store over db.
func NewGormDurableDedupStore(db *gorm.DB, opts ...DurableDedupOption) *GormDurableDedupStore {
	s := &GormDurableDedupStore{db: db, table: idempotencyKeysBaseTable, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// conn resolves the transaction-scoped *gorm.DB when ctx carries one (so
// Claim/Complete commit with the handler's tx), else the base db.
func (s *GormDurableDedupStore) conn(ctx context.Context) *gorm.DB {
	if h, ok := persistence.TxFromContext(ctx); ok {
		if tx, ok := h.(*gorm.DB); ok {
			return tx.WithContext(ctx)
		}
	}
	return s.db.WithContext(ctx)
}

// Lookup reads the LIVE (non-expired) record for key on the base db (no
// transaction) — the retry fast path. An expired record reports ok=false so the
// request re-executes.
func (s *GormDurableDedupStore) Lookup(ctx context.Context, key persistence.IdempotencyKey) (persistence.IdempotencyRecord, bool, error) {
	var row IdempotencyKeyRow
	err := s.db.WithContext(ctx).Table(s.table).
		Where("account_id = ? AND method = ? AND request_id = ? AND expires_at > ?",
			key.Tenant, key.Method, key.RequestID, s.now()).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return persistence.IdempotencyRecord{}, false, fmt.Errorf("gormtx: idempotency lookup: %w", err)
	}
	return toRecord(row), true, nil
}

// Claim inserts an in_progress row inside the ctx transaction. A fresh insert
// returns claimed=true. A conflict with a LIVE row returns that row and
// claimed=false. A conflict with an EXPIRED row reclaims it (guarded UPDATE) as a
// fresh in_progress claim.
//
// Assumes READ COMMITTED isolation (Postgres/MySQL default): after ON CONFLICT DO
// NOTHING reports no insert, the follow-up read must see the row a concurrent
// transaction just committed. Under REPEATABLE READ / SERIALIZABLE the tx snapshot
// can hide that row (or raise a serialization error) — run the idempotency
// transaction at READ COMMITTED.
func (s *GormDurableDedupStore) Claim(ctx context.Context, key persistence.IdempotencyKey, fingerprint string, ttl time.Duration) (persistence.IdempotencyRecord, bool, error) {
	db := s.conn(ctx)
	now := s.now()
	row := IdempotencyKeyRow{
		AccountID:   key.Tenant,
		Method:      key.Method,
		RequestID:   key.RequestID,
		Status:      string(persistence.StatusInProgress),
		Fingerprint: fingerprint,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	}
	// ON CONFLICT (primary key) DO NOTHING keeps the transaction alive on a
	// duplicate (a raw unique violation would poison a Postgres tx, blocking the
	// follow-up SELECT). The conflict target is stated explicitly so a future unique
	// index on the table is not silently swallowed by a bare "on any constraint".
	res := db.Table(s.table).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}, {Name: "method"}, {Name: "request_id"}},
		DoNothing: true,
	}).Create(&row)
	if res.Error != nil {
		return persistence.IdempotencyRecord{}, false, fmt.Errorf("gormtx: idempotency claim: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return persistence.IdempotencyRecord{}, true, nil // fresh claim
	}

	// Conflict: read the existing (now-committed) row.
	cur, err := s.take(ctx, db, key)
	if err != nil {
		return persistence.IdempotencyRecord{}, false, err
	}
	if !cur.ExpiresAt.After(now) {
		// Expired: reclaim it. The WHERE expires_at <= now admits exactly one racer.
		upd := db.Table(s.table).
			Where("account_id = ? AND method = ? AND request_id = ? AND expires_at <= ?",
				key.Tenant, key.Method, key.RequestID, now).
			Updates(map[string]any{
				"status":        string(persistence.StatusInProgress),
				"response_type": "",
				"response":      nil,
				"fingerprint":   fingerprint,
				"created_at":    now,
				"expires_at":    now.Add(ttl),
			})
		if upd.Error != nil {
			return persistence.IdempotencyRecord{}, false, fmt.Errorf("gormtx: idempotency reclaim: %w", upd.Error)
		}
		if upd.RowsAffected == 1 {
			return persistence.IdempotencyRecord{}, true, nil // reclaimed
		}
		// A concurrent racer reclaimed it first — re-read the live row.
		if cur, err = s.take(ctx, db, key); err != nil {
			return persistence.IdempotencyRecord{}, false, err
		}
	}
	return toRecord(cur), false, nil
}

// Complete transitions key's in_progress row to completed with the response, in
// the ctx transaction.
func (s *GormDurableDedupStore) Complete(ctx context.Context, key persistence.IdempotencyKey, responseType string, response []byte) error {
	res := s.conn(ctx).Table(s.table).
		Where("account_id = ? AND method = ? AND request_id = ?", key.Tenant, key.Method, key.RequestID).
		Updates(map[string]any{
			"status":        string(persistence.StatusCompleted),
			"response_type": responseType,
			"response":      response,
		})
	if res.Error != nil {
		return fmt.Errorf("gormtx: idempotency complete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("gormtx: idempotency complete: no claimed row for request_id %q", key.RequestID)
	}
	return nil
}

// GC deletes records whose expiry is at or before now, returning the count removed.
func (s *GormDurableDedupStore) GC(ctx context.Context, now time.Time) (int64, error) {
	res := s.db.WithContext(ctx).Table(s.table).
		Where("expires_at <= ?", now).Delete(&IdempotencyKeyRow{})
	if res.Error != nil {
		return 0, fmt.Errorf("gormtx: idempotency gc: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// take reads one row by its composite key.
func (s *GormDurableDedupStore) take(ctx context.Context, db *gorm.DB, key persistence.IdempotencyKey) (IdempotencyKeyRow, error) {
	var row IdempotencyKeyRow
	err := db.Table(s.table).
		Where("account_id = ? AND method = ? AND request_id = ?", key.Tenant, key.Method, key.RequestID).
		Take(&row).Error
	if err != nil {
		return IdempotencyKeyRow{}, fmt.Errorf("gormtx: idempotency read: %w", err)
	}
	return row, nil
}

func toRecord(r IdempotencyKeyRow) persistence.IdempotencyRecord {
	return persistence.IdempotencyRecord{
		Status:       persistence.IdempotencyStatus(r.Status),
		ResponseType: r.ResponseType,
		Response:     r.Response,
		Fingerprint:  r.Fingerprint,
	}
}
