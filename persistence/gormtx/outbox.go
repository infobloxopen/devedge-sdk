// outbox.go — the GORM-backed persistence.OutboxStore (F032/F033). It is the GORM
// analogue of the IAM fixture's EntOutboxStore and a sibling of the in-memory
// persistence.MemoryOutboxStore: a reusable, backend-neutral outbox table that
// any GORM service can mount.
//
// The critical method is Append: it resolves the transaction-scoped *gorm.DB
// carried on ctx (via persistence.TxFromContext, the F030 seam) and writes the
// outbox row THROUGH that transaction, so the row commits in the SAME GORM
// transaction as the aggregate change that emitted it and is discarded on
// rollback (F032 AC-1, the transactional-outbox guarantee). Claim/Release run on
// the base db because the background dispatcher is not inside an aggregate
// transaction.
//
// F033 (append-only + partitioned): the table is APPEND-ONLY — the store never
// DELETEs a row and MarkDelivered is a no-op (delivery truth is the idempotency
// marker, recorded in the handler's tx). Retention is whole-partition drops via
// DropPartitionsBefore (OutboxRetention), an O(1) DDL on a PostgreSQL declarative
// RANGE-on-created_time table created by EnsureOutboxPartitions — never a per-row
// DELETE. The partitioning DDL lives HERE in the store (a driver-aware package),
// keeping the persistence interfaces ORM/driver-neutral (clean core).
package gormtx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// OutboxRow is the GORM model for the transactional-outbox table. It mirrors the
// F032 D-2 schema (persistence.OutboxRecord) one-for-one. Payload is opaque
// bytes (a marshalled proto or JSON body) so the table stays codec-neutral;
// AccountID scopes the row to a tenant; Attempts/LeasedUntil are dispatcher
// bookkeeping for at-least-once delivery with a claim lease.
//
// F033: created_time is part of the primary key (id, created_time) so PostgreSQL
// declarative RANGE partitioning on created_time is legal (a partition key must be
// in every unique key). AutoMigrate produces a plain (non-partitioned) table good
// for SQLite/dev; EnsureOutboxPartitions builds the partitioned table on PG.
type OutboxRow struct {
	ID            string `gorm:"primaryKey;type:varchar(36)"`
	AccountID     string `gorm:"column:account_id;index"`
	AggregateType string `gorm:"column:aggregate_type"`
	AggregateID   string `gorm:"column:aggregate_id"`
	EventType     string `gorm:"column:event_type"`
	Payload       []byte `gorm:"column:payload"`
	// CreatedTime is immutable and is the RANGE partition key (F033). It is part of
	// the primary key so PG declarative partitioning on it is legal.
	CreatedTime time.Time `gorm:"primaryKey;column:created_time"`
	// Deprecated: F033 made the outbox append-only — delivery truth is the idempotency
	// marker, not a row write. Retained for field-compatibility; no longer written or
	// read on the dispatch path.
	DeliveredTime *time.Time `gorm:"column:delivered_time"`
	// Attempts counts delivery tries; ClaimUndelivered increments it on each claim and
	// a row past maxAttempts is no longer claimed (the poison cutoff).
	Attempts int `gorm:"column:attempts;index"`
	// LeasedUntil hides a claimed row from a competing claim until the lease lapses
	// (the safe-without-SKIP-LOCKED claim, F032 D-3).
	LeasedUntil *time.Time `gorm:"column:leased_until;index"`
}

// TableName pins the table to "outbox" regardless of struct/package naming.
func (OutboxRow) TableName() string { return "outbox" }

// GormOutboxStore is the GORM/SQL-backed persistence.OutboxStore and
// persistence.OutboxRetention.
//
// Claiming uses a lease (leased_until) rather than SELECT ... FOR UPDATE SKIP
// LOCKED: a row whose lease has not lapsed is hidden from a competing claim, so the
// default poller is safe without row-level locking (which the test SQLite dialector
// does not support anyway).
type GormOutboxStore struct {
	db       *gorm.DB
	leaseTTL time.Duration
	now      func() time.Time
}

// OutboxOption configures a GormOutboxStore.
type OutboxOption func(*GormOutboxStore)

// WithOutboxLeaseTTL sets the claim lease duration. A non-positive value keeps
// the default.
func WithOutboxLeaseTTL(d time.Duration) OutboxOption {
	return func(s *GormOutboxStore) {
		if d > 0 {
			s.leaseTTL = d
		}
	}
}

// withOutboxNow overrides the clock (used by tests to drive lease expiry).
func withOutboxNow(now func() time.Time) OutboxOption {
	return func(s *GormOutboxStore) {
		if now != nil {
			s.now = now
		}
	}
}

// NewGormOutboxStore returns a GORM-backed OutboxStore over db. Construct it with
// the same *gorm.DB the New<R>Repository constructors and GormTxRunner use, so
// Append resolves the same transaction handle they bind to.
func NewGormOutboxStore(db *gorm.DB, opts ...OutboxOption) *GormOutboxStore {
	s := &GormOutboxStore{db: db, leaseTTL: 30 * time.Second, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// conn resolves the transaction-scoped *gorm.DB when ctx carries one (so Append
// participates in the enclosing Atomically); otherwise the base db.
func (s *GormOutboxStore) conn(ctx context.Context) *gorm.DB {
	if h, ok := persistence.TxFromContext(ctx); ok {
		if tx, ok := h.(*gorm.DB); ok {
			return tx.WithContext(ctx)
		}
	}
	return s.db.WithContext(ctx)
}

// Append implements persistence.OutboxStore. It fails closed when ctx is not in a
// transaction (F032 D-1 backstop): an outbox row written outside the aggregate's
// commit would reintroduce the dual write the outbox exists to prevent. The row
// is written through the ctx tx so it commits with — or rolls back with — the
// aggregate change.
func (s *GormOutboxStore) Append(ctx context.Context, rec *persistence.OutboxRecord) error {
	if err := persistence.RequireTx(ctx); err != nil {
		return err
	}
	created := rec.CreatedTime
	if created.IsZero() {
		created = s.now()
	}
	row := &OutboxRow{
		ID:            rec.ID,
		AccountID:     rec.AccountID,
		AggregateType: rec.AggregateType,
		AggregateID:   rec.AggregateID,
		EventType:     rec.EventType,
		Payload:       rec.Payload,
		CreatedTime:   created,
		Attempts:      rec.Attempts,
	}
	if err := s.conn(ctx).Create(row).Error; err != nil {
		if ce := persistence.ConstraintError(err); ce != nil {
			return ce
		}
		return fmt.Errorf("append outbox row: %w", err)
	}
	return nil
}

// ClaimUndelivered implements persistence.OutboxStore: lease up to limit rows still
// eligible for dispatch (attempts < maxAttempts and lease lapsed), bumping attempts
// and stamping a fresh lease, and return them. Runs on the base db (the dispatcher
// is not in an aggregate tx).
//
// F033 churn-avoidance: a delivered row (delivered_time IS NOT NULL) is EXCLUDED, so
// a successfully delivered event is never re-leased or re-attempted — the happy path
// issues zero per-poll UPDATEs and a delivered event never drifts into the poison
// cutoff. A row past maxAttempts without ever delivering is poison and skipped (the
// poison cutoff). The handler idempotency markers remain the exactly-once guard for a
// rare in-flight double-claim; aged rows are removed by a partition drop.
func (s *GormOutboxStore) ClaimUndelivered(ctx context.Context, maxAttempts, limit int) ([]*persistence.OutboxRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if maxAttempts <= 0 {
		maxAttempts = persistence.DefaultMaxOutboxAttempts
	}
	now := s.now()
	var rows []OutboxRow
	err := s.db.WithContext(ctx).
		Where("delivered_time IS NULL AND attempts < ? AND (leased_until IS NULL OR leased_until < ?)", maxAttempts, now).
		Order("created_time ASC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("query claimable: %w", err)
	}
	out := make([]*persistence.OutboxRecord, 0, len(rows))
	lease := now.Add(s.leaseTTL)
	for i := range rows {
		r := rows[i]
		// Scope the lease update by the full PK (id, created_time): the partitioned
		// table's PK includes created_time, and matching it lets PG prune to the one
		// partition instead of scanning every partition.
		res := s.db.WithContext(ctx).Model(&OutboxRow{}).
			Where("id = ? AND created_time = ?", r.ID, r.CreatedTime).
			Updates(map[string]any{
				"leased_until": lease,
				"attempts":     r.Attempts + 1,
			})
		if res.Error != nil {
			return nil, fmt.Errorf("lease outbox row %s: %w", r.ID, res.Error)
		}
		out = append(out, fromOutboxRow(&r, r.Attempts+1))
	}
	return out, nil
}

// MarkDelivered implements persistence.OutboxStore: stamp delivered_time ONCE (and
// clear the lease) so the row is excluded from every future ClaimUndelivered — the
// single terminal mark that keeps the append-only outbox free of per-poll re-lease
// churn (F033). The WHERE delivered_time IS NULL makes a re-mark a no-op; the row is
// never DELETEd (retention is a partition drop). A row that no longer exists is a
// no-op. It runs on the base db (the dispatcher is not in an aggregate tx).
func (s *GormOutboxStore) MarkDelivered(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Model(&OutboxRow{}).
		Where("id = ? AND delivered_time IS NULL", id).
		Updates(map[string]any{
			"delivered_time": s.now(),
			"leased_until":   nil,
		})
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("mark delivered %s: %w", id, res.Error)
	}
	return nil
}

// Release implements persistence.OutboxStore: drop the lease so a re-claim is
// immediate (the prompt at-least-once retry path). A row that no longer exists is a
// no-op. It does NOT delete the row (append-only).
func (s *GormOutboxStore) Release(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Model(&OutboxRow{}).
		Where("id = ?", id).
		Update("leased_until", nil)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("release %s: %w", id, res.Error)
	}
	return nil
}

// fromOutboxRow maps the GORM model to the neutral persistence.OutboxRecord.
func fromOutboxRow(r *OutboxRow, attempts int) *persistence.OutboxRecord {
	return &persistence.OutboxRecord{
		ID:            r.ID,
		AccountID:     r.AccountID,
		AggregateType: r.AggregateType,
		AggregateID:   r.AggregateID,
		EventType:     r.EventType,
		Payload:       r.Payload,
		CreatedTime:   r.CreatedTime,
		Attempts:      attempts,
	}
}

// compile-time checks.
var (
	_ persistence.OutboxStore     = (*GormOutboxStore)(nil)
	_ persistence.OutboxRetention = (*GormOutboxStore)(nil)
)
