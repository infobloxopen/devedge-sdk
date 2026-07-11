// Hand-written (NOT protoc-gen-ent output): the WS-043 / F048 ent-backed durable,
// exactly-once request-idempotency store for the IAM fixture — the ent counterpart of
// gormtx.GormDurableDedupStore and the reference wiring for
// entrepo.EntDurableDedupStore.
//
// entrepo owns the exactly-once LOGIC; this file wires the six thin closures to the
// generated ent client, each binding to the handler's transaction through the ctx *ent.Tx
// (persistence.TxFromContext, the F030 seam) exactly like EntOutboxStore/EntIdempotencyStore.
// So Claim/Complete/Abandon commit atomically with the handler's ent aggregate write, and
// Lookup/GC run on the base client (no transaction).
package iamv1

import (
	"context"
	"errors"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
	ent "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	entidem "github.com/infobloxopen/devedge-sdk/testdata/iam/ent/idempotencykey"
)

// NewEntDurableDedupStore builds the ent-backed durable idempotency store for the IAM
// module over client, wiring entrepo.EntDurableDedupStore's closures to the generated ent
// IdempotencyKey client. Construct it with the same *ent.Client the handler's EntTxRunner
// uses so Claim/Complete/Abandon bind to the handler's transaction.
func NewEntDurableDedupStore(client *ent.Client, opts ...entrepo.EntDurableDedupOption) *entrepo.EntDurableDedupStore {
	// idemClient resolves the tx-bound IdempotencyKey client when ctx carries an *ent.Tx
	// (so the write participates in the enclosing Atomically); otherwise the base client
	// (the Lookup / GC fast path).
	idemClient := func(ctx context.Context) *ent.IdempotencyKeyClient {
		if h, ok := persistence.TxFromContext(ctx); ok {
			if tx, ok := h.(*ent.Tx); ok {
				return tx.IdempotencyKey
			}
		}
		return client.IdempotencyKey
	}

	s := entrepo.NewEntDurableDedupStore(opts...)

	s.InsertFn = func(ctx context.Context, row entrepo.EntIdempotencyRow) error {
		_, err := idemClient(ctx).Create().
			SetID(row.ID).
			SetAccountID(row.AccountID).
			SetMethod(row.Method).
			SetRequestID(row.RequestID).
			SetStatus(row.Status).
			SetFingerprint(row.Fingerprint).
			SetCreatedAt(row.CreatedAt).
			SetExpiresAt(row.ExpiresAt).
			Save(ctx)
		if err != nil {
			// A duplicate id (the concurrent-claim race) is a UNIQUE/PK violation; map ONLY
			// that to the neutral sentinel the store understands (which reports it as
			// in_progress → 409). persistence.ConstraintError is unique-violation-specific
			// (SQLSTATE 23505 / "duplicate key" / sqlite "UNIQUE constraint failed"), so a
			// different constraint (were one ever added) surfaces as a real error rather than
			// being masked as a 409. The idempotency_keys table has no FKs and Claim always
			// sets the NOT NULL columns, so only the PK unique violation can fire here.
			if ce := persistence.ConstraintError(err); errors.Is(ce, persistence.ErrConflict) {
				return persistence.ErrConflict
			}
			return err
		}
		return nil
	}

	s.ReadFn = func(ctx context.Context, id string) (entrepo.EntIdempotencyRow, bool, error) {
		r, err := idemClient(ctx).Get(ctx, id)
		if err != nil {
			if ent.IsNotFound(err) {
				return entrepo.EntIdempotencyRow{}, false, nil
			}
			return entrepo.EntIdempotencyRow{}, false, err
		}
		return fromEntIdem(r), true, nil
	}

	s.ReclaimFn = func(ctx context.Context, row entrepo.EntIdempotencyRow, now time.Time) (int64, error) {
		n, err := idemClient(ctx).Update().
			Where(entidem.ID(row.ID), entidem.ExpiresAtLTE(now)).
			SetStatus(row.Status).
			SetResponseType("").
			ClearResponse().
			SetFingerprint(row.Fingerprint).
			SetCreatedAt(row.CreatedAt).
			SetExpiresAt(row.ExpiresAt).
			Save(ctx)
		return int64(n), err
	}

	s.CompleteFn = func(ctx context.Context, id, responseType string, response []byte) (int64, error) {
		n, err := idemClient(ctx).Update().
			Where(entidem.ID(id)).
			SetStatus(string(persistence.StatusCompleted)).
			SetResponseType(responseType).
			SetResponse(response).
			Save(ctx)
		return int64(n), err
	}

	s.AbandonFn = func(ctx context.Context, id string) (int64, error) {
		n, err := idemClient(ctx).Delete().
			Where(entidem.ID(id), entidem.StatusEQ(string(persistence.StatusInProgress))).
			Exec(ctx)
		return int64(n), err
	}

	s.GCDeleteFn = func(ctx context.Context, now time.Time, limit int) (int64, error) {
		// Bounded chunk: select up to limit expired ids, then delete exactly those. Two
		// round-trips, but each is bounded (no giant table-locking DELETE) and runs on the
		// base client (GC is not inside a handler tx).
		ids, err := client.IdempotencyKey.Query().
			Where(entidem.ExpiresAtLTE(now)).
			Limit(limit).
			IDs(ctx)
		if err != nil {
			return 0, err
		}
		if len(ids) == 0 {
			return 0, nil
		}
		n, err := client.IdempotencyKey.Delete().
			Where(entidem.IDIn(ids...)).
			Exec(ctx)
		return int64(n), err
	}

	return s
}

// fromEntIdem maps a generated ent.IdempotencyKey to the neutral entrepo row.
func fromEntIdem(e *ent.IdempotencyKey) entrepo.EntIdempotencyRow {
	return entrepo.EntIdempotencyRow{
		ID:           e.ID,
		AccountID:    e.AccountID,
		Method:       e.Method,
		RequestID:    e.RequestID,
		Status:       e.Status,
		ResponseType: e.ResponseType,
		Response:     e.Response,
		Fingerprint:  e.Fingerprint,
		CreatedAt:    e.CreatedAt,
		ExpiresAt:    e.ExpiresAt,
	}
}
