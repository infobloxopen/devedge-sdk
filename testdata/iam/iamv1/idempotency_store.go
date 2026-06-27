// Hand-written (NOT protoc-gen-ent output): the F032 ent-backed
// events.IdempotencyStore for the IAM fixture. It is the ent twin of
// persistence/gormtx.GormIdempotencyStore and the SQL-backed replacement for the
// in-memory events.MemoryIdempotencyStore the ent dispatch path used to rely on.
//
// The gap it closes: the in-memory store's marker is NOT part of the handler's ent
// transaction, so on the ent path the idempotency marker committed (or not)
// independently of the handler's aggregate write — a narrow orphan-marker window.
// Here Record inserts the marker row THROUGH the handler's ctx *ent.Tx (resolved
// via persistence.TxFromContext, the F030 seam), so the marker commits ATOMICALLY
// with the handler's aggregate write and rolls back with it. A concurrent (or
// lapsed-lease) double-delivery races to insert the same primary key; exactly one
// transaction commits (effect + marker) and the other gets a PK/unique conflict,
// which Record maps to events.ErrAlreadyApplied so the duplicate effect rolls back
// (F032 AC-2).
package iamv1

import (
	"context"
	"errors"
	"fmt"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
	ent "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	entidem "github.com/infobloxopen/devedge-sdk/testdata/iam/ent/idemmarker"
)

// EntIdempotencyStore is the ent/SQL-backed events.IdempotencyStore for the IAM
// module. Construct it with the same *ent.Client the handler's EntTxRunner uses,
// so Record's marker insert binds to the handler's transaction and commits with
// the handler's aggregate write.
type EntIdempotencyStore struct {
	client *ent.Client
}

// NewEntIdempotencyStore returns an ent-backed IdempotencyStore over client.
func NewEntIdempotencyStore(client *ent.Client) *EntIdempotencyStore {
	return &EntIdempotencyStore{client: client}
}

// idemClient resolves the tx-bound IdemMarker client when ctx carries an *ent.Tx,
// so Record participates in the enclosing Atomically (the marker commits with the
// handler's tx); otherwise the base client (the Seen fast-path).
func (s *EntIdempotencyStore) idemClient(ctx context.Context) *ent.IdemMarkerClient {
	if h, ok := persistence.TxFromContext(ctx); ok {
		if tx, ok := h.(*ent.Tx); ok {
			return tx.IdemMarker
		}
	}
	return s.client.IdemMarker
}

// Seen implements events.IdempotencyStore: a fast-path pre-check on the base
// client that reports whether key has already been recorded. Correctness does NOT
// depend on Seen — the in-tx Record below is the real exactly-once guard.
func (s *EntIdempotencyStore) Seen(ctx context.Context, key string) (bool, error) {
	n, err := s.client.IdemMarker.Query().Where(entidem.ID(key)).Count(ctx)
	if err != nil {
		return false, fmt.Errorf("idempotency seen %q: %w", key, err)
	}
	return n > 0, nil
}

// Record implements events.IdempotencyStore: insert the marker INSIDE the
// handler's transaction (resolved from ctx), so the marker commits atomically with
// the handler's aggregate write. A duplicate key is a unique/PK constraint
// violation, surfaced as events.ErrAlreadyApplied so the dispatcher rolls the
// duplicate effect back — the side effect runs exactly once even under a
// double-delivery (F032 AC-2).
func (s *EntIdempotencyStore) Record(ctx context.Context, key string) error {
	if _, err := s.idemClient(ctx).Create().SetID(key).Save(ctx); err != nil {
		if ce := persistence.ConstraintError(err); errors.Is(ce, persistence.ErrConflict) {
			return events.ErrAlreadyApplied
		}
		if ent.IsConstraintError(err) {
			return events.ErrAlreadyApplied
		}
		return fmt.Errorf("idempotency record %q: %w", key, err)
	}
	return nil
}

// compile-time check.
var _ events.IdempotencyStore = (*EntIdempotencyStore)(nil)
