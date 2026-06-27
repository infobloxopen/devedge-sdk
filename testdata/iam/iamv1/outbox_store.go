// Hand-written (NOT protoc-gen-ent output): the F032 ent-backed OutboxStore for the
// IAM fixture. It is the SQL-backend analogue of persistence.MemoryOutboxStore.
package iamv1

import (
	"context"
	"fmt"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
	ent "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	entoutbox "github.com/infobloxopen/devedge-sdk/testdata/iam/ent/outbox"
)

// EntOutboxStore is the ent/SQL-backed persistence.OutboxStore for the IAM module.
//
// The critical method is Append: it resolves the *ent.Tx carried on ctx (via
// persistence.TxFromContext, the F030 seam) and writes the outbox row THROUGH that
// transaction, so the row commits in the SAME transaction as the aggregate change
// that emitted it and is discarded on rollback (F032 AC-1, the transactional-outbox
// guarantee). Claim/MarkDelivered/Release run on the base client because the
// background dispatcher is not inside an aggregate transaction.
//
// Claiming uses a lease (leased_until) rather than SELECT ... FOR UPDATE SKIP LOCKED
// (ent sql/lock is not enabled in this repo — F032 D-3): an undelivered row whose
// lease has not lapsed is hidden from a competing claim.
type EntOutboxStore struct {
	client   *ent.Client
	leaseTTL time.Duration
	now      func() time.Time
}

// NewEntOutboxStore returns an ent-backed OutboxStore over client. A non-positive
// leaseTTL uses a sane default.
func NewEntOutboxStore(client *ent.Client, leaseTTL time.Duration) *EntOutboxStore {
	if leaseTTL <= 0 {
		leaseTTL = 30 * time.Second
	}
	return &EntOutboxStore{client: client, leaseTTL: leaseTTL, now: time.Now}
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

// ClaimUndelivered implements persistence.OutboxStore: lease up to limit undelivered
// rows whose lease has lapsed, bumping attempts and stamping a fresh lease, and
// return them. Runs on the base client (the dispatcher is not in an aggregate tx).
func (s *EntOutboxStore) ClaimUndelivered(ctx context.Context, limit int) ([]*persistence.OutboxRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	now := s.now()
	rows, err := s.client.Outbox.Query().
		Where(
			entoutbox.DeliveredTimeIsNil(),
			entoutbox.Or(
				entoutbox.LeasedUntilIsNil(),
				entoutbox.LeasedUntilLT(now),
			),
		).
		Order(ent.Asc(entoutbox.FieldCreatedTime)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query undelivered: %w", err)
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

// MarkDelivered implements persistence.OutboxStore: stamp delivered_time (terminal)
// and clear the lease.
func (s *EntOutboxStore) MarkDelivered(ctx context.Context, id string) error {
	_, err := s.client.Outbox.UpdateOneID(id).
		SetDeliveredTime(s.now()).
		ClearLeasedUntil().
		Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return persistence.ErrNotFound
		}
		return fmt.Errorf("mark delivered %s: %w", id, err)
	}
	return nil
}

// Release implements persistence.OutboxStore: drop the lease so a re-claim is
// immediate (the prompt at-least-once retry path).
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
		DeliveredTime: e.DeliveredTime,
		Attempts:      attempts,
	}
}

// compile-time check.
var _ persistence.OutboxStore = (*EntOutboxStore)(nil)
