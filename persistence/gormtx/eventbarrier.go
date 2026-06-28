// eventbarrier.go — L4 event-plane barrier for cell-based development on the GORM
// backend. OutboxEventBarrier is the controller-facing cells.EventBarrier backed by
// the transactional outbox + a per-tenant policy table:
//
//   - SetPolicy upserts the publisher mode (NORMAL / PAUSE / DRAIN_QUEUE) and the
//     event epoch for a tenant in the framework tenant_event_policy table. Forward-only
//     on the epoch (cells.ErrFenceRegression on a backward epoch).
//
//   - Drained reports whether the tenant has no PENDING outbox rows at an event_epoch
//     at or below the barrier — i.e. the relay has published everything the source
//     produced up to the barrier. The controller waits on this before committing a move.
//
// "Pending" is "not yet past the relay's forward cursor": the relay advances one global
// cursor in commit order, so a tenant is drained when every one of its rows at
// event_epoch ≤ barrier sorts at or before the relay's cursor position. This reuses the
// write-only outbox + the existing sidecar cursor; it does NOT add per-row delivery
// state to the outbox.
package gormtx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/infobloxopen/devedge-sdk/cells"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// TenantEventPolicyRow is the framework tenant_event_policy table: the per-tenant
// publisher mode + event epoch for the L4 event plane. Policy stores the
// cells.EventPolicy as an int; EventEpoch is forward-only.
type TenantEventPolicyRow struct {
	TenantID   string `gorm:"primaryKey;column:tenant_id;type:varchar(255)"`
	Policy     int    `gorm:"column:policy;default:0"`
	EventEpoch int64  `gorm:"column:event_epoch;default:0"`
	UpdatedAt  time.Time
}

// TableName pins the policy table name.
func (TenantEventPolicyRow) TableName() string { return "tenant_event_policy" }

// eventPolicyBaseTable is the unqualified policy table name.
const eventPolicyBaseTable = "tenant_event_policy"

// OutboxEventBarrier is the GORM-backed cells.EventBarrier. It records the per-tenant
// publisher policy in tenant_event_policy and answers Drained from the write-only
// outbox + the relay's sidecar cursor. Construct it with the same *gorm.DB the outbox
// store and the relay's cursor store use.
type OutboxEventBarrier struct {
	db          *gorm.DB
	now         func() time.Time
	policyTable string
	outboxTable string
	cursors     persistence.OutboxCursorStore
	cursorName  string
}

// EventBarrierOption configures an OutboxEventBarrier.
type EventBarrierOption func(*OutboxEventBarrier)

// WithBarrierNamespace qualifies the policy + outbox tables per a module's
// persistence.DatabaseNamespace (parity with the outbox/cursor stores). The zero
// namespace leaves the bare names.
func WithBarrierNamespace(ns persistence.DatabaseNamespace) EventBarrierOption {
	return func(b *OutboxEventBarrier) {
		b.policyTable = ns.QualifyTable(eventPolicyBaseTable)
		b.outboxTable = ns.QualifyTable(outboxBaseTable)
	}
}

// WithBarrierCursors sets the relay's sidecar cursor store + name so Drained can read
// the relay's forward position. When unset, Drained treats the relay as at the start
// of stream (nothing published yet), so a tenant with any pending row reports
// not-drained — the safe default. The controller should pass the SAME cursor store
// and name the relay advances.
func WithBarrierCursors(cursors persistence.OutboxCursorStore, cursorName string) EventBarrierOption {
	return func(b *OutboxEventBarrier) {
		b.cursors = cursors
		if cursorName != "" {
			b.cursorName = cursorName
		}
	}
}

// DefaultRelayCursorName mirrors events.DefaultCursorName for the barrier's default
// cursor lookup without importing events (avoids a dependency direction surprise; the
// barrier lives next to the outbox store).
const DefaultRelayCursorName = "default"

// NewOutboxEventBarrier returns a GORM-backed EventBarrier over db.
func NewOutboxEventBarrier(db *gorm.DB, opts ...EventBarrierOption) *OutboxEventBarrier {
	b := &OutboxEventBarrier{
		db:          db,
		now:         time.Now,
		policyTable: eventPolicyBaseTable,
		outboxTable: outboxBaseTable,
		cursorName:  DefaultRelayCursorName,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// SetPolicy implements cells.EventBarrier: upsert {Policy, EventEpoch} for tenantID.
// Forward-only: cells.ErrFenceRegression when eventEpoch is below the stored epoch.
// Idempotent on the same epoch.
func (b *OutboxEventBarrier) SetPolicy(ctx context.Context, tenantID string, policy cells.EventPolicy, eventEpoch uint64) error {
	return b.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur TenantEventPolicyRow
		err := tx.Table(b.policyTable).Where("tenant_id = ?", tenantID).Take(&cur).Error
		switch {
		case err == nil:
			if eventEpoch < uint64(cur.EventEpoch) {
				return cells.ErrFenceRegression
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
		default:
			return fmt.Errorf("read event policy for %q: %w", tenantID, err)
		}
		row := TenantEventPolicyRow{
			TenantID:   tenantID,
			Policy:     int(policy),
			EventEpoch: int64(eventEpoch),
			UpdatedAt:  b.now(),
		}
		if serr := tx.Table(b.policyTable).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "tenant_id"}},
			UpdateAll: true,
		}).Create(&row).Error; serr != nil {
			return fmt.Errorf("write event policy for %q: %w", tenantID, serr)
		}
		return nil
	})
}

// Drained implements cells.EventBarrier: report whether the tenant has no PENDING
// outbox rows at event_epoch ≤ eventEpoch — i.e. the relay has published everything
// the source produced up to the barrier. A row is pending when it sorts STRICTLY
// AFTER the relay's forward cursor in (created_time, id) order (the relay publishes
// in commit order and advances that cursor; everything at or before it is published).
//
// A tenant with zero such rows (event-free, or all already published) is drained.
func (b *OutboxEventBarrier) Drained(ctx context.Context, tenantID string, eventEpoch uint64) (bool, error) {
	cursor := persistence.OutboxCursor{}
	if b.cursors != nil {
		c, _, err := b.cursors.LoadCursor(ctx, b.cursorName)
		if err != nil {
			return false, fmt.Errorf("load relay cursor: %w", err)
		}
		cursor = c
	}
	q := b.db.WithContext(ctx).Table(b.outboxTable).
		Where("account_id = ?", tenantID).
		Where("event_epoch <= ?", int64(eventEpoch))
	if !cursor.IsZero() {
		// Pending = sorts strictly after the relay cursor in (created_time, id) order.
		q = q.Where("created_time > ? OR (created_time = ? AND id > ?)",
			cursor.CreatedTime, cursor.CreatedTime, cursor.ID)
	}
	var pending int64
	if err := q.Count(&pending).Error; err != nil {
		return false, fmt.Errorf("count pending outbox rows for %q: %w", tenantID, err)
	}
	return pending == 0, nil
}

// Policy returns the stored publisher policy + event epoch for tenantID (introspection
// / tests). A tenant with no row reports (PolicyNormal, 0).
func (b *OutboxEventBarrier) Policy(ctx context.Context, tenantID string) (cells.EventPolicy, uint64, error) {
	var row TenantEventPolicyRow
	err := b.db.WithContext(ctx).Table(b.policyTable).Where("tenant_id = ?", tenantID).Take(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return cells.PolicyNormal, 0, nil
	case err != nil:
		return cells.PolicyNormal, 0, fmt.Errorf("read event policy for %q: %w", tenantID, err)
	}
	return cells.EventPolicy(row.Policy), uint64(row.EventEpoch), nil
}

// compile-time check.
var _ cells.EventBarrier = (*OutboxEventBarrier)(nil)
