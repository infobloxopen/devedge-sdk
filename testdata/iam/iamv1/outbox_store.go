// Hand-written (NOT protoc-gen-ent output): the F032/F033 ent-backed OutboxStore for
// the IAM fixture. It is the SQL-backend analogue of persistence.MemoryOutboxStore.
//
// F033 WRITE-ONLY: the table is write-only. Append writes through the ctx *ent.Tx (the
// producer's transactional insert), ReadAfter is a non-mutating forward scan
// (`(created_time, id) > cursor`) the in-process dispatcher consumes, and the store
// NEVER UPDATEs or DELETEs a row on the dispatch path — there are no delivered_time /
// attempts / leased_until columns and no claim/lease/mark methods. The dispatcher keeps
// its position in a SIDECAR (EntOutboxCursorStore), not in the outbox. Retention is
// whole-partition drops via DropPartitionsBefore (persistence.OutboxRetention): on
// PostgreSQL/MySQL it issues partition DDL (DROP/ALTER TABLE ... DROP PARTITION, O(1));
// on the sqlite dev backend it degrades to a windowed delete of aged rows.
package iamv1

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
	ent "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	entoutbox "github.com/infobloxopen/devedge-sdk/testdata/iam/ent/outbox"
	entcursor "github.com/infobloxopen/devedge-sdk/testdata/iam/ent/outboxcursor"
)

// EntOutboxStore is the ent/SQL-backed persistence.OutboxStore + OutboxRetention for
// the IAM module. It is WRITE-ONLY: Append inserts through the ctx *ent.Tx and
// ReadAfter scans forward; nothing mutates a row.
//
// The critical method is Append: it resolves the *ent.Tx carried on ctx (via
// persistence.TxFromContext, the F030 seam) and writes the outbox row THROUGH that
// transaction, so the row commits in the SAME transaction as the aggregate change that
// emitted it and is discarded on rollback (F032 AC-1). ReadAfter runs on the base
// client because the in-process dispatcher is not inside an aggregate tx.
type EntOutboxStore struct {
	client *ent.Client
	now    func() time.Time
	// rawDB, when set, is the underlying *sql.DB used ONLY for partition-drop retention
	// DDL on a partitioned SQL backend (ent does not expose declarative partitioning).
	// It is never touched on the dispatch path. Inject it with WithEntOutboxRawDB.
	rawDB *sql.DB
	// mysql selects the MySQL partition-drop DDL (ALTER TABLE ... DROP PARTITION) over
	// the default PostgreSQL DDL when rawDB is set. Set it with WithEntOutboxMySQL.
	mysql bool
}

// EntOutboxOption configures an EntOutboxStore.
type EntOutboxOption func(*EntOutboxStore)

// WithEntOutboxRawDB injects the underlying *sql.DB used for PostgreSQL partition-drop
// retention DDL (DropPartitionsBefore). It is only needed on a partitioned PG/MySQL
// deployment; the sqlite dev backend does not require it.
func WithEntOutboxRawDB(db *sql.DB) EntOutboxOption {
	return func(s *EntOutboxStore) { s.rawDB = db }
}

// WithEntOutboxMySQL selects the MySQL partition-drop DDL for DropPartitionsBefore
// (ALTER TABLE ... DROP PARTITION) instead of the default PostgreSQL DDL. Combine it
// with WithEntOutboxRawDB on a partitioned MySQL deployment.
func WithEntOutboxMySQL() EntOutboxOption {
	return func(s *EntOutboxStore) { s.mysql = true }
}

// NewEntOutboxStore returns an ent-backed write-only OutboxStore over client. The
// second argument is retained for source compatibility (it was a lease TTL; the
// write-only store has no lease) and is ignored.
func NewEntOutboxStore(client *ent.Client, _ time.Duration, opts ...EntOutboxOption) *EntOutboxStore {
	s := &EntOutboxStore{client: client, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// outboxClient resolves the tx-bound Outbox client when ctx carries an *ent.Tx, so
// Append participates in the enclosing Atomically; otherwise the base client.
func (s *EntOutboxStore) outboxClient(ctx context.Context) *ent.OutboxClient {
	if h, ok := persistence.TxFromContext(ctx); ok {
		if tx, ok := h.(*ent.Tx); ok {
			return tx.Outbox
		}
	}
	return s.client.Outbox
}

// Append implements persistence.OutboxStore. It fails closed when ctx is not in a
// transaction (F032 D-1 backstop): an outbox row written outside the aggregate's commit
// would reintroduce the dual write the outbox exists to prevent. This is the ONLY write
// the producer makes to the outbox.
func (s *EntOutboxStore) Append(ctx context.Context, rec *persistence.OutboxRecord) error {
	if err := persistence.RequireTx(ctx); err != nil {
		return err
	}
	created := rec.CreatedTime
	if created.IsZero() {
		created = s.now()
	}
	b := s.outboxClient(ctx).Create().
		SetID(rec.ID).
		SetAccountID(rec.AccountID).
		SetAggregateType(rec.AggregateType).
		SetAggregateID(rec.AggregateID).
		SetEventType(rec.EventType).
		SetPayload(rec.Payload).
		SetCreatedTime(created)
	if _, err := b.Save(ctx); err != nil {
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
// its position in the sidecar and never writes back here. Runs on the base client (the
// dispatcher is not in an aggregate tx).
func (s *EntOutboxStore) ReadAfter(ctx context.Context, cursor persistence.OutboxCursor, limit int) ([]*persistence.OutboxRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	q := s.client.Outbox.Query()
	if !cursor.IsZero() {
		// Keyset predicate: created_time > c OR (created_time = c AND id > c.id) — a total
		// order over (created_time, id), so the scan never skips or repeats a row.
		q = q.Where(entoutbox.Or(
			entoutbox.CreatedTimeGT(cursor.CreatedTime),
			entoutbox.And(
				entoutbox.CreatedTimeEQ(cursor.CreatedTime),
				entoutbox.IDGT(cursor.ID),
			),
		))
	}
	rows, err := q.
		Order(ent.Asc(entoutbox.FieldCreatedTime), ent.Asc(entoutbox.FieldID)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("read outbox after cursor: %w", err)
	}
	out := make([]*persistence.OutboxRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromEntOutbox(r))
	}
	return out, nil
}

// DropPartitionsBefore implements persistence.OutboxRetention.
//
// On PostgreSQL/MySQL (rawDB injected, see WithEntOutboxRawDB) it drops every monthly
// partition of the outbox whose ENTIRE window is older than t — a whole-partition DROP
// (O(1) DDL), never a per-row DELETE, so retention does not churn the heap (F033 AC-2).
// Otherwise (the sqlite dev backend) it models the same contract via a windowed delete
// of aged rows through the ent client — acceptable for dev/test only.
func (s *EntOutboxStore) DropPartitionsBefore(ctx context.Context, t time.Time) (int, error) {
	if s.rawDB != nil {
		if s.mysql {
			return dropEntMySQLPartitionsBefore(ctx, s.rawDB, t)
		}
		return dropEntPGPartitionsBefore(ctx, s.rawDB, t)
	}
	// Dev/test backend: no partitions, so "drop a partition" degrades to forgetting
	// rows older than t. This is NOT on the dispatch loop (retention task only).
	n, err := s.client.Outbox.Delete().
		Where(entoutbox.CreatedTimeLT(t)).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("drop outbox rows before %s: %w", t.Format(time.RFC3339), err)
	}
	return n, nil
}

// fromEntOutbox maps a generated ent.Outbox to the neutral persistence.OutboxRecord.
func fromEntOutbox(e *ent.Outbox) *persistence.OutboxRecord {
	return &persistence.OutboxRecord{
		ID:            e.ID,
		AccountID:     e.AccountID,
		AggregateType: e.AggregateType,
		AggregateID:   e.AggregateID,
		EventType:     e.EventType,
		Payload:       e.Payload,
		CreatedTime:   e.CreatedTime,
	}
}

// --- sidecar cursor store (the dispatcher's own progress, NEVER the outbox) ---

// EntOutboxCursorStore is the ent-backed persistence.OutboxCursorStore sidecar for the
// in-process dispatcher. The outbox is write-only, so the dispatcher records its
// forward position, head-of-line failure count, and any dead-lettered poison events
// HERE (the OutboxCursor / OutboxDeadLetter ent entities), never in the outbox.
type EntOutboxCursorStore struct {
	client *ent.Client
	now    func() time.Time
}

// NewEntOutboxCursorStore returns an ent-backed cursor sidecar over client.
func NewEntOutboxCursorStore(client *ent.Client) *EntOutboxCursorStore {
	return &EntOutboxCursorStore{client: client, now: time.Now}
}

// LoadCursor implements persistence.OutboxCursorStore: return the saved position +
// head-failure count for name (zero/0 if never saved).
func (s *EntOutboxCursorStore) LoadCursor(ctx context.Context, name string) (persistence.OutboxCursor, int, error) {
	row, err := s.client.OutboxCursor.Query().Where(entcursor.ID(name)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return persistence.OutboxCursor{}, 0, nil
		}
		return persistence.OutboxCursor{}, 0, fmt.Errorf("load cursor %q: %w", name, err)
	}
	return persistence.OutboxCursor{CreatedTime: row.CursorTime, ID: row.CursorID}, row.HeadFailures, nil
}

// SaveCursor implements persistence.OutboxCursorStore: upsert the named cursor's
// position + head-failure count. The upsert feature is not enabled on this ent client,
// so it updates the existing row and falls back to a create when the row does not yet
// exist (a single dispatcher owns the cursor, so there is no write contention).
func (s *EntOutboxCursorStore) SaveCursor(ctx context.Context, name string, cursor persistence.OutboxCursor, headFailures int) error {
	n, err := s.client.OutboxCursor.Update().
		Where(entcursor.ID(name)).
		SetCursorTime(cursor.CreatedTime).
		SetCursorID(cursor.ID).
		SetHeadFailures(headFailures).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("update cursor %q: %w", name, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := s.client.OutboxCursor.Create().
		SetID(name).
		SetCursorTime(cursor.CreatedTime).
		SetCursorID(cursor.ID).
		SetHeadFailures(headFailures).
		Save(ctx); err != nil {
		if ce := persistence.ConstraintError(err); ce != nil {
			// A concurrent first-save won the create; retry the update path once.
			if _, uerr := s.client.OutboxCursor.Update().
				Where(entcursor.ID(name)).
				SetCursorTime(cursor.CreatedTime).
				SetCursorID(cursor.ID).
				SetHeadFailures(headFailures).
				Save(ctx); uerr != nil {
				return fmt.Errorf("save cursor %q (retry): %w", name, uerr)
			}
			return nil
		}
		return fmt.Errorf("create cursor %q: %w", name, err)
	}
	return nil
}

// DeadLetter implements persistence.OutboxCursorStore: park a poison event in the
// sidecar dead-letter table.
func (s *EntOutboxCursorStore) DeadLetter(ctx context.Context, name string, rec *persistence.OutboxRecord, reason string) error {
	if _, err := s.client.OutboxDeadLetter.Create().
		SetCursorName(name).
		SetEventID(rec.ID).
		SetEventType(rec.EventType).
		SetReason(reason).
		SetCreatedTime(rec.CreatedTime).
		SetRecordedAt(s.now()).
		Save(ctx); err != nil {
		return fmt.Errorf("dead-letter %s: %w", rec.ID, err)
	}
	return nil
}

// compile-time checks.
var (
	_ persistence.OutboxStore       = (*EntOutboxStore)(nil)
	_ persistence.OutboxRetention   = (*EntOutboxStore)(nil)
	_ persistence.OutboxCursorStore = (*EntOutboxCursorStore)(nil)
)
