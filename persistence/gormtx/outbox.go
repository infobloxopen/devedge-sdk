// outbox.go — the GORM-backed persistence.OutboxStore (F032). It is the GORM
// analogue of the IAM fixture's EntOutboxStore and a sibling of the in-memory
// persistence.MemoryOutboxStore: a reusable, backend-neutral outbox table that
// any GORM service can mount.
//
// The critical method is Append: it resolves the transaction-scoped *gorm.DB
// carried on ctx (via persistence.TxFromContext, the F030 seam) and writes the
// outbox row THROUGH that transaction, so the row commits in the SAME GORM
// transaction as the aggregate change that emitted it and is discarded on
// rollback (F032 AC-1, the transactional-outbox guarantee). Claim/MarkDelivered/
// Release run on the base db because the background dispatcher is not inside an
// aggregate transaction.
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
// AccountID scopes the row to a tenant; DeliveredTime/Attempts/LeasedUntil are
// dispatcher bookkeeping for at-least-once delivery with a claim lease.
type OutboxRow struct {
	ID            string `gorm:"primaryKey;type:varchar(36)"`
	AccountID     string `gorm:"column:account_id;index"`
	AggregateType string `gorm:"column:aggregate_type"`
	AggregateID   string `gorm:"column:aggregate_id"`
	EventType     string `gorm:"column:event_type"`
	Payload       []byte `gorm:"column:payload"`
	CreatedTime   time.Time
	// DeliveredTime is nil until a dispatcher has delivered the event to every
	// handler; a non-nil value marks the row delivered (a terminal state).
	DeliveredTime *time.Time `gorm:"column:delivered_time;index"`
	// Attempts counts delivery tries; ClaimUndelivered increments it on each claim.
	Attempts int `gorm:"column:attempts"`
	// LeasedUntil hides a claimed-but-undelivered row from a competing claim until
	// the lease lapses (the safe-without-SKIP-LOCKED claim, F032 D-3).
	LeasedUntil *time.Time `gorm:"column:leased_until;index"`
}

// TableName pins the table to "outbox" regardless of struct/package naming.
func (OutboxRow) TableName() string { return "outbox" }

// GormOutboxStore is the GORM/SQL-backed persistence.OutboxStore.
//
// Claiming uses a lease (leased_until) rather than SELECT ... FOR UPDATE SKIP
// LOCKED: an undelivered row whose lease has not lapsed is hidden from a
// competing claim, so the default poller is safe without row-level locking
// (which the test SQLite dialector does not support anyway).
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
		DeliveredTime: rec.DeliveredTime,
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

// ClaimUndelivered implements persistence.OutboxStore: lease up to limit
// undelivered rows whose lease has lapsed, bumping attempts and stamping a fresh
// lease, and return them. Runs on the base db (the dispatcher is not in an
// aggregate tx).
func (s *GormOutboxStore) ClaimUndelivered(ctx context.Context, limit int) ([]*persistence.OutboxRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	now := s.now()
	var rows []OutboxRow
	err := s.db.WithContext(ctx).
		Where("delivered_time IS NULL AND (leased_until IS NULL OR leased_until < ?)", now).
		Order("created_time ASC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("query undelivered: %w", err)
	}
	out := make([]*persistence.OutboxRecord, 0, len(rows))
	lease := now.Add(s.leaseTTL)
	for i := range rows {
		r := rows[i]
		res := s.db.WithContext(ctx).Model(&OutboxRow{}).
			Where("id = ?", r.ID).
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

// MarkDelivered implements persistence.OutboxStore: stamp delivered_time
// (terminal) and clear the lease.
func (s *GormOutboxStore) MarkDelivered(ctx context.Context, id string) error {
	now := s.now()
	res := s.db.WithContext(ctx).Model(&OutboxRow{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"delivered_time": now,
			"leased_until":   nil,
		})
	if res.Error != nil {
		return fmt.Errorf("mark delivered %s: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// Release implements persistence.OutboxStore: drop the lease so a re-claim is
// immediate (the prompt at-least-once retry path). A row that no longer exists is
// a no-op, mirroring the ent store.
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
		DeliveredTime: r.DeliveredTime,
		Attempts:      attempts,
	}
}

// compile-time check.
var _ persistence.OutboxStore = (*GormOutboxStore)(nil)
