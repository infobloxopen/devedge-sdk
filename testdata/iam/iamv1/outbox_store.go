// Hand-written (NOT protoc-gen-ent output): the F032/F033 ent-backed OutboxStore for
// the IAM fixture. It is the SQL-backend analogue of persistence.MemoryOutboxStore.
//
// F033 (append-only + partitioned): the table is APPEND-ONLY — Append writes through
// the ctx *ent.Tx, ClaimUndelivered filters on attempts (NOT delivered_time),
// MarkDelivered is a no-op (delivery truth is the idempotency marker), and the store
// never DELETEs a row on the dispatch path. Retention is whole-partition drops via
// DropPartitionsBefore (persistence.OutboxRetention): on PostgreSQL it issues
// partition DDL (DROP TABLE of an aged monthly partition, O(1)); on the sqlite dev
// backend it degrades to a windowed delete of aged rows.
package iamv1

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
	ent "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	entoutbox "github.com/infobloxopen/devedge-sdk/testdata/iam/ent/outbox"
)

// EntOutboxStore is the ent/SQL-backed persistence.OutboxStore + OutboxRetention for
// the IAM module.
//
// The critical method is Append: it resolves the *ent.Tx carried on ctx (via
// persistence.TxFromContext, the F030 seam) and writes the outbox row THROUGH that
// transaction, so the row commits in the SAME transaction as the aggregate change
// that emitted it and is discarded on rollback (F032 AC-1). Claim/Release run on the
// base client because the background dispatcher is not inside an aggregate tx.
//
// Claiming uses a lease (leased_until) rather than SELECT ... FOR UPDATE SKIP LOCKED
// (ent sql/lock is not enabled in this repo — F032 D-3): a row whose lease has not
// lapsed is hidden from a competing claim.
type EntOutboxStore struct {
	client   *ent.Client
	leaseTTL time.Duration
	now      func() time.Time
	// rawDB, when set, is the underlying *sql.DB used ONLY for partition-drop retention
	// DDL on PostgreSQL (ent does not expose declarative partitioning). It is never
	// touched on the dispatch path. Inject it with WithEntOutboxRawDB on PG.
	rawDB *sql.DB
}

// EntOutboxOption configures an EntOutboxStore.
type EntOutboxOption func(*EntOutboxStore)

// WithEntOutboxRawDB injects the underlying *sql.DB used for PostgreSQL
// partition-drop retention DDL (DropPartitionsBefore). It is only needed on a
// partitioned PG deployment; the sqlite dev backend does not require it.
func WithEntOutboxRawDB(db *sql.DB) EntOutboxOption {
	return func(s *EntOutboxStore) { s.rawDB = db }
}

// NewEntOutboxStore returns an ent-backed OutboxStore over client. A non-positive
// leaseTTL uses a sane default.
func NewEntOutboxStore(client *ent.Client, leaseTTL time.Duration, opts ...EntOutboxOption) *EntOutboxStore {
	if leaseTTL <= 0 {
		leaseTTL = 30 * time.Second
	}
	s := &EntOutboxStore{client: client, leaseTTL: leaseTTL, now: time.Now}
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
// transaction (F032 D-1 backstop): an outbox row written outside the aggregate's
// commit would reintroduce the dual write the outbox exists to prevent.
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
		SetCreatedTime(created).
		SetAttempts(rec.Attempts)
	if _, err := b.Save(ctx); err != nil {
		if ce := persistence.ConstraintError(err); ce != nil {
			return ce
		}
		return fmt.Errorf("append outbox row: %w", err)
	}
	return nil
}

// ClaimUndelivered implements persistence.OutboxStore: lease up to limit rows still
// eligible for dispatch (attempts < maxAttempts and lease lapsed), bumping attempts
// and stamping a fresh lease, and return them. Runs on the base client (the
// dispatcher is not in an aggregate tx).
//
// F033: eligibility is attempts-based, NOT delivered_time-based. A row past
// maxAttempts is poison and skipped (the poison cutoff); a delivered row stays
// eligible until the cutoff but its re-delivery is a no-op (the handler idempotency
// markers dedup it) and it is eventually aged out by a partition drop.
func (s *EntOutboxStore) ClaimUndelivered(ctx context.Context, maxAttempts, limit int) ([]*persistence.OutboxRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if maxAttempts <= 0 {
		maxAttempts = persistence.DefaultMaxOutboxAttempts
	}
	now := s.now()
	rows, err := s.client.Outbox.Query().
		Where(
			entoutbox.AttemptsLT(maxAttempts),
			entoutbox.Or(
				entoutbox.LeasedUntilIsNil(),
				entoutbox.LeasedUntilLT(now),
			),
		).
		Order(ent.Asc(entoutbox.FieldCreatedTime)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query claimable: %w", err)
	}
	out := make([]*persistence.OutboxRecord, 0, len(rows))
	lease := now.Add(s.leaseTTL)
	for _, r := range rows {
		if _, uerr := s.client.Outbox.UpdateOneID(r.ID).
			SetLeasedUntil(lease).
			SetAttempts(r.Attempts + 1).
			Save(ctx); uerr != nil {
			return nil, fmt.Errorf("lease outbox row %s: %w", r.ID, uerr)
		}
		out = append(out, fromEntOutbox(r, r.Attempts+1))
	}
	return out, nil
}

// MarkDelivered implements persistence.OutboxStore: a NO-OP under the F033
// append-only model. Delivery truth is the idempotency marker recorded in the
// handler's tx, not a row write; the store never mutates delivery state and never
// deletes a row.
func (s *EntOutboxStore) MarkDelivered(ctx context.Context, id string) error {
	return nil
}

// Release implements persistence.OutboxStore: drop the lease so a re-claim is
// immediate (the prompt at-least-once retry path). It does NOT delete the row
// (append-only).
func (s *EntOutboxStore) Release(ctx context.Context, id string) error {
	_, err := s.client.Outbox.UpdateOneID(id).
		ClearLeasedUntil().
		Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil // nothing to release
		}
		return fmt.Errorf("release %s: %w", id, err)
	}
	return nil
}

// DropPartitionsBefore implements persistence.OutboxRetention.
//
// On PostgreSQL (rawDB injected, see WithEntOutboxRawDB) it drops every monthly
// partition of the outbox whose ENTIRE window is older than t — a whole-partition
// DROP TABLE (O(1) DDL), never a per-row DELETE, so retention does not churn the heap
// (F033 AC-2). Otherwise (the sqlite dev backend) it models the same contract via a
// windowed delete of aged rows through the ent client — acceptable for dev/test only.
func (s *EntOutboxStore) DropPartitionsBefore(ctx context.Context, t time.Time) (int, error) {
	if s.rawDB != nil {
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
func fromEntOutbox(e *ent.Outbox, attempts int) *persistence.OutboxRecord {
	return &persistence.OutboxRecord{
		ID:            e.ID,
		AccountID:     e.AccountID,
		AggregateType: e.AggregateType,
		AggregateID:   e.AggregateID,
		EventType:     e.EventType,
		Payload:       e.Payload,
		CreatedTime:   e.CreatedTime,
		Attempts:      attempts,
	}
}

// compile-time checks.
var (
	_ persistence.OutboxStore     = (*EntOutboxStore)(nil)
	_ persistence.OutboxRetention = (*EntOutboxStore)(nil)
)
