// dedupstore.go — WS-043 / F048 Increment 3, Deliverable C: the ent-backed durable,
// exactly-once request-idempotency store, closing the gap that only the GORM backend
// (gormtx.GormDurableDedupStore) implemented middleware.DurableIdempotencyStore.
//
// It follows the entrepo adapter pattern (like EntRepository): this package owns the
// exactly-once LOGIC (claim → live/expired conflict → reclaim → replay decision; batched
// GC loop), and a service wires thin CLOSURES that execute one statement each against its
// generated ent client, binding to the on-ctx *ent.Tx via persistence.TxFromContext
// (exactly like EntOutboxStore/EntIdempotencyStore bind). Because the logic lives once
// here, the ent path is structurally identical to gormtx — parity by construction — and
// the framework interceptor still only CALLS the interface.
//
// ent constraints shaped the design (see spec DC-2): ent 0.14 has no composite natural
// primary key and exposes no raw-SQL accessor on a transaction, so the backing table uses
// a single `id` primary key that ENCODES the full key (account_id\x00method\x00request_id,
// the proven IdemMarker trick). id uniqueness ≡ (account_id, method, request_id)
// uniqueness ⇒ exactly-once is preserved; account_id stays a real column so WS-029 RLS
// still covers it. Claim reads-then-inserts (rather than gorm's INSERT ON CONFLICT DO
// NOTHING) because the upsert feature is not required for correctness and a plain create
// keeps the ent wiring simple; a genuine concurrent fresh duplicate that races the insert
// is reported as in_progress (the caller returns 409) and the client's retry replays the
// winner's committed response via the non-transactional fast path — exactly-once holds,
// one extra round-trip versus gorm's in-call coalesce.
package entrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// DefaultEntGCBatchSize is the chunk size the batched GC deletes per round-trip.
const DefaultEntGCBatchSize = 1000

// idKeySep separates the key components in the encoded single-column id. It is a NUL byte
// (never present in a gRPC method, an account id, or an AIP-155 request_id), so the encode
// is unambiguous and collision-free.
const idKeySep = "\x00"

// EncodeIdempotencyID returns the single-column primary-key value that encodes the full
// tenant-scoped, per-operation key. A service's closures use the same encoding for the
// entity's id, so id uniqueness ≡ (account_id, method, request_id) uniqueness.
//
// Injectivity depends on no component containing the NUL separator; callers validate that
// via [checkKeyEncodable] before this is used as a durable key (Tenant is the verified
// principal and Method is the server-set gRPC method — both NUL-free in practice; the guard
// is defense-in-depth against a future caller feeding a client-controlled component).
func EncodeIdempotencyID(key persistence.IdempotencyKey) string {
	return key.Tenant + idKeySep + key.Method + idKeySep + key.RequestID
}

// checkKeyEncodable fails loud if a key component contains the NUL separator, which would
// make the encoded id ambiguous (a cross-boundary collision). It is a cheap invariant guard
// so the exactly-once fence cannot be silently subverted by a future caller.
func checkKeyEncodable(key persistence.IdempotencyKey) error {
	if strings.ContainsRune(key.Tenant, 0) || strings.ContainsRune(key.Method, 0) || strings.ContainsRune(key.RequestID, 0) {
		return fmt.Errorf("entrepo: idempotency key component contains a NUL byte (tenant/method/request_id must be NUL-free)")
	}
	return nil
}

// EntIdempotencyRow is the neutral row the service closures translate to/from its
// generated ent.IdempotencyKey entity. ID is the encoded composite key (the entity PK);
// the split columns are carried for querying, RLS (account_id), and replay.
type EntIdempotencyRow struct {
	ID           string
	AccountID    string
	Method       string
	RequestID    string
	Status       string
	ResponseType string
	Response     []byte
	Fingerprint  string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// EntDurableDedupStore is the generic ent-backed durable idempotency store. Construct it
// with New*; wire the closures to the module's ent client (see the iam fixture
// dedup_store.go for the reference). It satisfies middleware.DurableIdempotencyStore.
//
// Each write closure MUST bind to the *ent.Tx on ctx via persistence.TxFromContext (so
// Claim/Complete/Abandon commit atomically with the handler's ent effect); the read
// closure binds tx-if-present and falls back to the base client (the non-transactional
// Lookup / GC fast path).
type EntDurableDedupStore struct {
	// InsertFn inserts a fresh in_progress row in the ctx transaction. It returns
	// persistence.ErrConflict (or an error wrapping it) when the id already exists — the
	// only signal the store needs; any other error is surfaced as-is.
	InsertFn func(ctx context.Context, row EntIdempotencyRow) error
	// ReadFn reads the row by its encoded id, binding to the ctx tx when present else the
	// base client. ok is false when no row exists.
	ReadFn func(ctx context.Context, id string) (row EntIdempotencyRow, ok bool, err error)
	// ReclaimFn reclaims an EXPIRED row as a fresh in_progress claim in the ctx tx:
	// UPDATE ... WHERE id = row.ID AND expires_at <= now. It returns rows affected (1 = we
	// reclaimed it, 0 = a racer reclaimed first).
	ReclaimFn func(ctx context.Context, row EntIdempotencyRow, now time.Time) (affected int64, err error)
	// CompleteFn transitions the in_progress row to completed with the response, in the
	// ctx tx. It returns rows affected (0 = no claimed row).
	CompleteFn func(ctx context.Context, id, responseType string, response []byte) (affected int64, err error)
	// AbandonFn deletes the in_progress reservation in the ctx tx, guarded to
	// status = in_progress so it never erases a completed response. Returns rows affected.
	AbandonFn func(ctx context.Context, id string) (affected int64, err error)
	// GCDeleteFn deletes up to limit expired rows on the base client and returns the count
	// removed (the batched-GC primitive).
	GCDeleteFn func(ctx context.Context, now time.Time, limit int) (deleted int64, err error)

	now     func() time.Time
	gcBatch int
}

// EntDurableDedupOption configures an EntDurableDedupStore.
type EntDurableDedupOption func(*EntDurableDedupStore)

// WithEntDurableDedupClock overrides the store's clock (for TTL/GC tests).
func WithEntDurableDedupClock(now func() time.Time) EntDurableDedupOption {
	return func(s *EntDurableDedupStore) { s.now = now }
}

// WithEntDurableDedupGCBatch overrides the batched-GC chunk size (default
// [DefaultEntGCBatchSize]); a non-positive value keeps the default.
func WithEntDurableDedupGCBatch(n int) EntDurableDedupOption {
	return func(s *EntDurableDedupStore) {
		if n > 0 {
			s.gcBatch = n
		}
	}
}

// NewEntDurableDedupStore returns a store over the supplied closures. All six closures are
// required; a nil closure fails loud on first use.
func NewEntDurableDedupStore(opts ...EntDurableDedupOption) *EntDurableDedupStore {
	s := &EntDurableDedupStore{now: time.Now, gcBatch: DefaultEntGCBatchSize}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *EntDurableDedupStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// wired fails loud if any closure was left nil, so a mis-wired store returns a clear error
// instead of a raw nil-func panic on first use (the closures are set post-construction).
func (s *EntDurableDedupStore) wired() error {
	if s.InsertFn == nil || s.ReadFn == nil || s.ReclaimFn == nil ||
		s.CompleteFn == nil || s.AbandonFn == nil || s.GCDeleteFn == nil {
		return fmt.Errorf("entrepo: EntDurableDedupStore is missing one or more closures (InsertFn/ReadFn/ReclaimFn/CompleteFn/AbandonFn/GCDeleteFn)")
	}
	return nil
}

// Lookup reads the live (non-expired) record for key without a transaction (the retry
// fast path). An expired record reports ok=false so the request re-executes.
//
// ReadFn binds to an on-ctx *ent.Tx when present, but Lookup is only ever called by the
// interceptor's fast path OUTSIDE Atomically (no tx on ctx), so it reads the committed base
// state — matching the gorm store, which reads its base db unconditionally.
func (s *EntDurableDedupStore) Lookup(ctx context.Context, key persistence.IdempotencyKey) (persistence.IdempotencyRecord, bool, error) {
	if err := s.wired(); err != nil {
		return persistence.IdempotencyRecord{}, false, err
	}
	if err := checkKeyEncodable(key); err != nil {
		return persistence.IdempotencyRecord{}, false, err
	}
	row, ok, err := s.ReadFn(ctx, EncodeIdempotencyID(key))
	if err != nil {
		return persistence.IdempotencyRecord{}, false, fmt.Errorf("entrepo: idempotency lookup: %w", err)
	}
	if !ok || !row.ExpiresAt.After(s.clock()) {
		return persistence.IdempotencyRecord{}, false, nil
	}
	return toEntRecord(row), true, nil
}

// Claim reserves an in_progress record for key inside the ctx transaction. It mirrors the
// gorm store: a fresh key is inserted (claimed=true); a LIVE existing record is returned
// (claimed=false, the caller replays or 409s); an EXPIRED record is reclaimed. A genuine
// concurrent fresh duplicate that races our insert is reported as in_progress
// (claimed=false) so the caller 409s and the client's retry replays via Lookup.
func (s *EntDurableDedupStore) Claim(ctx context.Context, key persistence.IdempotencyKey, fingerprint string, ttl time.Duration) (persistence.IdempotencyRecord, bool, error) {
	if err := s.wired(); err != nil {
		return persistence.IdempotencyRecord{}, false, err
	}
	if err := checkKeyEncodable(key); err != nil {
		return persistence.IdempotencyRecord{}, false, err
	}
	now := s.clock()
	id := EncodeIdempotencyID(key)
	row := EntIdempotencyRow{
		ID:          id,
		AccountID:   key.Tenant,
		Method:      key.Method,
		RequestID:   key.RequestID,
		Status:      string(persistence.StatusInProgress),
		Fingerprint: fingerprint,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	}

	cur, ok, err := s.ReadFn(ctx, id)
	if err != nil {
		return persistence.IdempotencyRecord{}, false, fmt.Errorf("entrepo: idempotency read: %w", err)
	}
	if ok {
		if cur.ExpiresAt.After(now) {
			return toEntRecord(cur), false, nil // live conflict
		}
		// Expired: reclaim it. WHERE expires_at <= now admits exactly one racer.
		affected, rerr := s.ReclaimFn(ctx, row, now)
		if rerr != nil {
			return persistence.IdempotencyRecord{}, false, fmt.Errorf("entrepo: idempotency reclaim: %w", rerr)
		}
		if affected == 1 {
			return persistence.IdempotencyRecord{}, true, nil // reclaimed
		}
		// A racer reclaimed first — re-read the live row. If it VANISHED between the lost
		// reclaim and this read (only reachable if a concurrent Abandon deleted the racer's
		// fresh in_progress reservation), surface an error so the caller rolls back and
		// retries — matching the gorm store, whose take-after-lost-reclaim returns the
		// not-found error too. Either way exactly-once holds (the retry re-claims).
		cur, ok, err = s.ReadFn(ctx, id)
		if err != nil {
			return persistence.IdempotencyRecord{}, false, fmt.Errorf("entrepo: idempotency reread: %w", err)
		}
		if !ok {
			return persistence.IdempotencyRecord{}, false, fmt.Errorf("entrepo: idempotency reread: reclaimed row vanished for request_id %q", key.RequestID)
		}
		return toEntRecord(cur), false, nil
	}

	// Absent: insert a fresh claim. A concurrent inserter blocks us on the unique key; when
	// it commits we unique-violate → report in_progress so the caller 409s and the client's
	// retry replays the winner's committed response (exactly-once preserved).
	//
	// IMPORTANT (PostgreSQL): a unique-violation aborts the transaction, so on the conflict
	// branch we return WITHOUT issuing any further statement in this tx; the interceptor
	// returns immediately (409) and EntTxRunner.Atomically only issues ROLLBACK (always safe
	// on an aborted tx). A future caller must not run another statement in the same tx after
	// Claim reports claimed=false here.
	if ierr := s.InsertFn(ctx, row); ierr != nil {
		if errors.Is(ierr, persistence.ErrConflict) {
			return persistence.IdempotencyRecord{Status: persistence.StatusInProgress}, false, nil
		}
		return persistence.IdempotencyRecord{}, false, fmt.Errorf("entrepo: idempotency claim insert: %w", ierr)
	}
	return persistence.IdempotencyRecord{}, true, nil // fresh claim
}

// Complete transitions key's in_progress row to completed with the response, in the ctx tx.
func (s *EntDurableDedupStore) Complete(ctx context.Context, key persistence.IdempotencyKey, responseType string, response []byte) error {
	if err := s.wired(); err != nil {
		return err
	}
	affected, err := s.CompleteFn(ctx, EncodeIdempotencyID(key), responseType, response)
	if err != nil {
		return fmt.Errorf("entrepo: idempotency complete: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("entrepo: idempotency complete: no claimed row for request_id %q", key.RequestID)
	}
	return nil
}

// Abandon deletes key's in_progress reservation inside the ctx tx (the reserve→remote→
// complete release-on-error path). Guarded to status = in_progress so it never erases a
// completed response. Returns whether a row was deleted.
func (s *EntDurableDedupStore) Abandon(ctx context.Context, key persistence.IdempotencyKey) (bool, error) {
	if err := s.wired(); err != nil {
		return false, err
	}
	affected, err := s.AbandonFn(ctx, EncodeIdempotencyID(key))
	if err != nil {
		return false, fmt.Errorf("entrepo: idempotency abandon: %w", err)
	}
	return affected > 0, nil
}

// GC deletes records whose expiry is at or before now, returning the total removed. Like
// the gorm store it deletes in bounded CHUNKS (via GCDeleteFn's LIMIT) until a chunk
// removes fewer than the batch, so a large expired backlog never becomes one giant DELETE.
// A mid-loop error returns the count deleted so far plus the error.
func (s *EntDurableDedupStore) GC(ctx context.Context, now time.Time) (int64, error) {
	if err := s.wired(); err != nil {
		return 0, err
	}
	batch := s.gcBatch
	if batch <= 0 {
		batch = DefaultEntGCBatchSize
	}
	var total int64
	for {
		deleted, err := s.GCDeleteFn(ctx, now, batch)
		total += deleted
		if err != nil {
			return total, fmt.Errorf("entrepo: idempotency gc: %w", err)
		}
		if deleted < int64(batch) {
			return total, nil
		}
	}
}

func toEntRecord(r EntIdempotencyRow) persistence.IdempotencyRecord {
	return persistence.IdempotencyRecord{
		Status:       persistence.IdempotencyStatus(r.Status),
		ResponseType: r.ResponseType,
		Response:     r.Response,
		Fingerprint:  r.Fingerprint,
	}
}
