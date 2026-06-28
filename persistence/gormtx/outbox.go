// outbox.go — the GORM-backed persistence.OutboxStore (F032/F033). It is the GORM
// analogue of the IAM fixture's EntOutboxStore and a sibling of the in-memory
// persistence.MemoryOutboxStore: a reusable, backend-neutral, WRITE-ONLY outbox table
// that any GORM service can mount.
//
// The critical method is Append: it resolves the transaction-scoped *gorm.DB carried
// on ctx (via persistence.TxFromContext, the F030 seam) and writes the outbox row
// THROUGH that transaction, so the row commits in the SAME GORM transaction as the
// aggregate change that emitted it and is discarded on rollback (F032 AC-1, the
// transactional-outbox guarantee).
//
// F033 WRITE-ONLY: the table is write-only. The ONLY writes are Append (the producer's
// transactional insert) and DropPartitionsBefore (whole-partition retention DDL,
// OutboxRetention). The store NEVER UPDATEs or DELETEs an individual row — there are no
// delivered_time / attempts / leased_until columns and no claim/lease/mark methods. The
// in-process dispatcher reads rows forward via ReadAfter (a non-mutating
// `(created_time, id) > cursor` scan) and keeps its position in a SIDECAR
// (GormOutboxCursorStore), so a delivered event is never re-touched. Retention is
// whole-partition drops via DropPartitionsBefore on a PostgreSQL/MySQL declarative
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

// OutboxRow is the GORM model for the WRITE-ONLY transactional-outbox table. It mirrors
// the F033 schema (persistence.OutboxRecord) one-for-one. Payload is opaque bytes (a
// marshalled proto or JSON body) so the table stays codec-neutral; AccountID scopes the
// row to a tenant.
//
// F033: there are NO dispatcher-bookkeeping columns (no delivered_time, attempts, or
// leased_until) — the table is written once and read forward. created_time is part of
// the primary key (id, created_time) so PostgreSQL/MySQL declarative RANGE partitioning
// on created_time is legal (a partition key must be in every unique key), and it is the
// forward-cursor sort key. AutoMigrate produces a plain (non-partitioned) table good
// for SQLite/dev; EnsureOutboxPartitions builds the partitioned table on PG/MySQL.
type OutboxRow struct {
	ID            string `gorm:"primaryKey;type:varchar(36)"`
	AccountID     string `gorm:"column:account_id;index"`
	AggregateType string `gorm:"column:aggregate_type"`
	AggregateID   string `gorm:"column:aggregate_id"`
	EventType     string `gorm:"column:event_type"`
	Payload       []byte `gorm:"column:payload"`
	// CreatedTime is immutable and is the RANGE partition key (F033) and the forward
	// cursor's primary sort key. It is part of the primary key so PG/MySQL declarative
	// partitioning on it is legal.
	CreatedTime time.Time `gorm:"primaryKey;column:created_time;index"`
}

// TableName pins the table to "outbox" regardless of struct/package naming.
func (OutboxRow) TableName() string { return "outbox" }

// GormOutboxStore is the GORM/SQL-backed persistence.OutboxStore and
// persistence.OutboxRetention. It is WRITE-ONLY: Append inserts (through the ctx tx)
// and ReadAfter scans forward; nothing mutates a row.
//
// WS-012 P2: the store's table name is RESOLVED at construction from a
// persistence.DatabaseNamespace, not taken from the model's pinned TableName, so
// two co-resident modules sharing one database get isolated outbox tables
// (orders.outbox vs billing.outbox under schema isolation; ord_outbox vs
// bil_outbox under prefix isolation). The zero namespace yields the historical
// bare "outbox", so a single-module service is unchanged.
type GormOutboxStore struct {
	db    *gorm.DB
	now   func() time.Time
	table string // resolved (possibly namespaced) outbox table name
}

// OutboxOption configures a GormOutboxStore.
type OutboxOption func(*GormOutboxStore)

// withOutboxNow overrides the clock (used by tests for deterministic created times).
func withOutboxNow(now func() time.Time) OutboxOption {
	return func(s *GormOutboxStore) {
		if now != nil {
			s.now = now
		}
	}
}

// WithOutboxNamespace qualifies the store's outbox table per a module's
// persistence.DatabaseNamespace (WS-012 P2). With a Schema set, the table is
// "schema.outbox" (reached via search_path or an explicit schema); with a
// TablePrefix set, it is "prefix"+"outbox". The zero namespace leaves the bare
// "outbox" name, so existing single-module services are unaffected.
func WithOutboxNamespace(ns persistence.DatabaseNamespace) OutboxOption {
	return func(s *GormOutboxStore) {
		s.table = ns.QualifyTable(outboxBaseTable)
	}
}

// outboxBaseTable is the unqualified outbox table name (OutboxRow.TableName).
const outboxBaseTable = "outbox"

// NewGormOutboxStore returns a GORM-backed write-only OutboxStore over db. Construct it
// with the same *gorm.DB the New<R>Repository constructors and GormTxRunner use, so
// Append resolves the same transaction handle they bind to. Without a
// WithOutboxNamespace option the store uses the bare "outbox" table (single-module
// behavior, unchanged).
func NewGormOutboxStore(db *gorm.DB, opts ...OutboxOption) *GormOutboxStore {
	s := &GormOutboxStore{db: db, now: time.Now, table: outboxBaseTable}
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
// transaction (F032 D-1 backstop): an outbox row written outside the aggregate's commit
// would reintroduce the dual write the outbox exists to prevent. The row is written
// through the ctx tx so it commits with — or rolls back with — the aggregate change.
// This is the ONLY write the producer makes to the outbox.
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
	}
	if err := s.conn(ctx).Table(s.table).Create(row).Error; err != nil {
		if ce := persistence.ConstraintError(err); ce != nil {
			return ce
		}
		return fmt.Errorf("append outbox row: %w", err)
	}
	return nil
}

// ReadAfter implements persistence.OutboxStore: return up to limit rows strictly after
// cursor in (created_time, id) order, WITHOUT mutating any row. It is the
// non-destructive forward scan the in-process dispatcher consumes; the dispatcher keeps
// its position in a sidecar and never writes back here. Runs on the base db (the
// dispatcher is not in an aggregate tx).
//
// The keyset predicate `created_time > c OR (created_time = c AND id > c.id)` is a
// total order over (created_time, id), so the scan never skips or repeats a row, and on
// a partitioned table created_time ordering lets PG/MySQL prune partitions.
func (s *GormOutboxStore) ReadAfter(ctx context.Context, cursor persistence.OutboxCursor, limit int) ([]*persistence.OutboxRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Table(s.table).Model(&OutboxRow{})
	if !cursor.IsZero() {
		q = q.Where("created_time > ? OR (created_time = ? AND id > ?)", cursor.CreatedTime, cursor.CreatedTime, cursor.ID)
	}
	var rows []OutboxRow
	if err := q.Order("created_time ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read outbox after cursor: %w", err)
	}
	out := make([]*persistence.OutboxRecord, 0, len(rows))
	for i := range rows {
		out = append(out, fromOutboxRow(&rows[i]))
	}
	return out, nil
}

// fromOutboxRow maps the GORM model to the neutral persistence.OutboxRecord.
func fromOutboxRow(r *OutboxRow) *persistence.OutboxRecord {
	return &persistence.OutboxRecord{
		ID:            r.ID,
		AccountID:     r.AccountID,
		AggregateType: r.AggregateType,
		AggregateID:   r.AggregateID,
		EventType:     r.EventType,
		Payload:       r.Payload,
		CreatedTime:   r.CreatedTime,
	}
}

// --- sidecar cursor store (the dispatcher's own progress, NEVER the outbox) ---

// OutboxCursorRow is the GORM model for the dispatcher's sidecar cursor table. The
// outbox is write-only, so the in-process dispatcher records its forward position
// (created_time, id), its head-of-line failure count, and (in OutboxDeadLetterRow) any
// poison events HERE, not in the outbox. One row per named cursor.
type OutboxCursorRow struct {
	Name string `gorm:"primaryKey;column:name;type:varchar(255)"`
	// CursorTime must store at least the same precision as the outbox created_time so
	// the keyset (`created_time > cursor_time`) round-trips the just-consumed head event
	// exactly — a lower-precision column would truncate the cursor and re-read the
	// consumed event as "after" the cursor (a re-delivery loop). PostgreSQL (timestamptz)
	// and SQLite keep full precision by default. On MySQL the default time column is
	// datetime(3) but the partitioned outbox created_time is datetime(6); a MySQL
	// deployment that wires the in-process dispatcher must migrate cursor_time as
	// datetime(6) (e.g. an explicit `ALTER TABLE outbox_dispatch_cursor MODIFY cursor_time
	// datetime(6)` after AutoMigrate) so the precisions match — the SDK keeps the model
	// dialect-portable rather than emitting a non-portable column type here.
	CursorTime   time.Time `gorm:"column:cursor_time"`
	CursorID     string    `gorm:"column:cursor_id"`
	HeadFailures int       `gorm:"column:head_failures"`
}

// TableName pins the sidecar table name.
func (OutboxCursorRow) TableName() string { return "outbox_dispatch_cursor" }

// OutboxDeadLetterRow is the GORM model for a parked poison event in the sidecar: an
// event that failed delivery maxAttempts times at the cursor head before the dispatcher
// advanced past it. It is auditable/replayable; the outbox row itself is untouched.
type OutboxDeadLetterRow struct {
	ID          uint      `gorm:"primaryKey;autoIncrement"`
	CursorName  string    `gorm:"column:cursor_name;index"`
	EventID     string    `gorm:"column:event_id"`
	EventType   string    `gorm:"column:event_type"`
	Reason      string    `gorm:"column:reason"`
	CreatedTime time.Time `gorm:"column:created_time"`
	RecordedAt  time.Time `gorm:"column:recorded_at"`
}

// TableName pins the dead-letter table name.
func (OutboxDeadLetterRow) TableName() string { return "outbox_dead_letter" }

// GormOutboxCursorStore is the GORM-backed persistence.OutboxCursorStore sidecar for
// the in-process dispatcher. It is DISTINCT from the write-only outbox: all dispatcher
// progress (cursor position, head-failure count, dead-letters) lives in its own tables.
//
// WS-012 P2: its two table names are RESOLVED from a persistence.DatabaseNamespace
// (WithCursorNamespace), so two co-resident modules get isolated dispatcher
// sidecars and never share one cursor row. The zero namespace yields the bare
// names, so single-module behavior is unchanged.
type GormOutboxCursorStore struct {
	db              *gorm.DB
	now             func() time.Time
	cursorTable     string
	deadLetterTable string
}

// cursorBaseTable / deadLetterBaseTable are the unqualified sidecar table names.
const (
	cursorBaseTable     = "outbox_dispatch_cursor"
	deadLetterBaseTable = "outbox_dead_letter"
)

// CursorOption configures a GormOutboxCursorStore.
type CursorOption func(*GormOutboxCursorStore)

// WithCursorNamespace qualifies the dispatcher's cursor + dead-letter tables per a
// module's persistence.DatabaseNamespace (WS-012 P2). The zero namespace leaves
// the bare names.
func WithCursorNamespace(ns persistence.DatabaseNamespace) CursorOption {
	return func(s *GormOutboxCursorStore) {
		s.cursorTable = ns.QualifyTable(cursorBaseTable)
		s.deadLetterTable = ns.QualifyTable(deadLetterBaseTable)
	}
}

// NewGormOutboxCursorStore returns a GORM-backed cursor sidecar over db. AutoMigrate
// &OutboxCursorRow{} and &OutboxDeadLetterRow{} so the sidecar tables exist (or,
// under a namespace, create them in the module schema / with the module prefix).
func NewGormOutboxCursorStore(db *gorm.DB, opts ...CursorOption) *GormOutboxCursorStore {
	s := &GormOutboxCursorStore{
		db:              db,
		now:             time.Now,
		cursorTable:     cursorBaseTable,
		deadLetterTable: deadLetterBaseTable,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// LoadCursor implements persistence.OutboxCursorStore: return the saved position +
// head-failure count for name (zero/0 if never saved).
func (s *GormOutboxCursorStore) LoadCursor(ctx context.Context, name string) (persistence.OutboxCursor, int, error) {
	var row OutboxCursorRow
	err := s.db.WithContext(ctx).Table(s.cursorTable).Where("name = ?", name).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return persistence.OutboxCursor{}, 0, nil
		}
		return persistence.OutboxCursor{}, 0, fmt.Errorf("load cursor %q: %w", name, err)
	}
	return persistence.OutboxCursor{CreatedTime: row.CursorTime, ID: row.CursorID}, row.HeadFailures, nil
}

// SaveCursor implements persistence.OutboxCursorStore: upsert the named cursor's
// position + head-failure count.
func (s *GormOutboxCursorStore) SaveCursor(ctx context.Context, name string, cursor persistence.OutboxCursor, headFailures int) error {
	row := OutboxCursorRow{
		Name:         name,
		CursorTime:   cursor.CreatedTime,
		CursorID:     cursor.ID,
		HeadFailures: headFailures,
	}
	// Upsert on the primary key (name): insert the first time, update thereafter.
	err := s.db.WithContext(ctx).Table(s.cursorTable).Save(&row).Error
	if err != nil {
		return fmt.Errorf("save cursor %q: %w", name, err)
	}
	return nil
}

// DeadLetter implements persistence.OutboxCursorStore: park a poison event in the
// sidecar dead-letter table.
func (s *GormOutboxCursorStore) DeadLetter(ctx context.Context, name string, rec *persistence.OutboxRecord, reason string) error {
	row := OutboxDeadLetterRow{
		CursorName:  name,
		EventID:     rec.ID,
		EventType:   rec.EventType,
		Reason:      reason,
		CreatedTime: rec.CreatedTime,
		RecordedAt:  s.now(),
	}
	if err := s.db.WithContext(ctx).Table(s.deadLetterTable).Create(&row).Error; err != nil {
		return fmt.Errorf("dead-letter %s: %w", rec.ID, err)
	}
	return nil
}

// compile-time checks.
var (
	_ persistence.OutboxStore       = (*GormOutboxStore)(nil)
	_ persistence.OutboxRetention   = (*GormOutboxStore)(nil)
	_ persistence.OutboxCursorStore = (*GormOutboxCursorStore)(nil)
)
