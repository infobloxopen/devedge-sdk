// idempotency.go — the DURABLE, exactly-once request-idempotency path (WS-043 /
// F048), the transactional upgrade of the best-effort in-memory DeduplicateUnary
// (F023). Where the memory store caches the response AFTER the handler returns
// (per-pod, TTL-only, non-atomic), the durable path claims an idempotency record
// INSIDE the handler's transaction and completes it in the same commit — so a
// committed effect always has a retrievable response, a retry that lands on
// another pod or after a restart replays the ORIGINAL response verbatim
// (including server-generated ids/etag), and a concurrent duplicate is a conflict
// rather than a second execution.
//
// The store persists opaque response BYTES + the proto message name; this file
// owns the proto marshal/unmarshal and the registry lookup, so the persistence
// adapter (persistence/gormtx) stays a pure row store. The interceptor opens the
// outer persistence.TxRunner.Atomically; the generated CRUD handler's repository
// write NEST-JOINS it (GormTxRunner reuses an on-ctx tx), so claim + effect +
// completion commit as one unit.
package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// DefaultIdempotencyTTL is the retention applied to a durable idempotency record
// when DurableDedup.TTL is zero. ~24h matches OCI/Stripe/AWS client-token
// retention.
const DefaultIdempotencyTTL = 24 * time.Hour

// The durable idempotency data types (IdempotencyKey, IdempotencyRecord,
// IdempotencyStatus + the status constants) live in package persistence so an
// adapter can implement the store without importing this gRPC middleware layer.
// They are re-exported here for callers that already import middleware.
type (
	// IdempotencyKey aliases persistence.IdempotencyKey.
	IdempotencyKey = persistence.IdempotencyKey
	// IdempotencyRecord aliases persistence.IdempotencyRecord.
	IdempotencyRecord = persistence.IdempotencyRecord
	// IdempotencyStatus aliases persistence.IdempotencyStatus.
	IdempotencyStatus = persistence.IdempotencyStatus
)

const (
	// StatusInProgress re-exports persistence.StatusInProgress.
	StatusInProgress = persistence.StatusInProgress
	// StatusCompleted re-exports persistence.StatusCompleted.
	StatusCompleted = persistence.StatusCompleted
)

// DurableIdempotencyStore is the durable backing store for
// [DurableDeduplicateUnary]. Claim and Complete MUST bind to the transaction on
// ctx (persistence.TxFromContext) so they commit atomically with the handler's
// effect; Lookup is a non-transactional fast-path read. Implementations live in a
// persistence adapter (e.g. persistence/gormtx.GormDurableDedupStore).
type DurableIdempotencyStore interface {
	// Lookup returns the live (non-expired) record for key without a transaction.
	// ok is false when no live record exists.
	Lookup(ctx context.Context, key IdempotencyKey) (rec IdempotencyRecord, ok bool, err error)
	// Claim inserts an in_progress record for key inside the ctx transaction,
	// setting the given fingerprint and an expiry of now+ttl. On a fresh claim it
	// returns claimed=true. If a LIVE record already exists it returns that record
	// with claimed=false (the caller decides replay vs. conflict). An EXPIRED
	// conflicting record is reclaimed as a fresh in_progress claim.
	Claim(ctx context.Context, key IdempotencyKey, fingerprint string, ttl time.Duration) (existing IdempotencyRecord, claimed bool, err error)
	// Complete transitions key's in_progress record to completed with the response
	// bytes + proto type name, inside the ctx transaction.
	Complete(ctx context.Context, key IdempotencyKey, responseType string, response []byte) error
	// Abandon deletes key's in_progress reservation inside the ctx transaction,
	// releasing it so a retry re-executes. It is the release-on-error step of the
	// reserve→remote→complete saga path ([DurableReserveUnary]); the transactional
	// path never calls it (it rolls the claim back with the effect instead). It MUST
	// be guarded to status = in_progress so it can never erase a completed (durable)
	// response. It returns whether a row was deleted (false when the record was
	// already completed or gone).
	Abandon(ctx context.Context, key IdempotencyKey) (bool, error)
	// GC deletes records whose expiry is at or before now, returning the count
	// removed. Intended for a periodic sweep; correctness does not depend on it
	// (Lookup/Claim already treat expired records as absent).
	GC(ctx context.Context, now time.Time) (int64, error)
}

// DurableDedupMode selects which durable interceptor a [DurableDedup] configures.
type DurableDedupMode int

const (
	// DurableModeTransactional (the default) claims, runs the handler, and completes
	// the record inside ONE transaction ([DurableDeduplicateUnary]) — exactly-once for
	// a LOCAL DB effect that nest-joins the same transaction.
	DurableModeTransactional DurableDedupMode = iota
	// DurableModeReserve reserves (claims + commits) an in_progress record, runs the
	// handler OUTSIDE any transaction (the REMOTE effect), then completes the record in
	// a second short transaction ([DurableReserveUnary]) — so no DB connection or claim
	// row lock is held across the remote call.
	DurableModeReserve
)

// DurableDedup groups the wiring for the durable idempotency path. Set it on
// server.Config to select [DurableDeduplicateUnary] over the best-effort memory
// path. Store and Tx are required.
//
// PRECONDITIONS (exactly-once holds only when all are met):
//   - The handler's domain write MUST participate in the transaction this
//     interceptor opens — i.e. it writes through an SDK repository / TxRunner over
//     the SAME backend as Store and Tx (the generated GORM/ent repositories do this
//     by resolving their connection from persistence.TxFromContext). A handler that
//     writes via a different *gorm.DB, a raw *sql.DB, or a Tx pointed at a different
//     database commits its effect independently — then a rolled-back claim
//     double-applies, or a committed claim replays a phantom success. This is NOT
//     enforced at runtime; it is the caller's contract.
//   - Use it for LOCAL, fast, DB-effect handlers. Because the handler runs inside
//     the transaction, a handler that makes a slow remote call holds a DB connection
//     and the claim row lock for its whole duration, and the remote effect is NOT
//     covered by the rollback. Remote-effect handlers want the reserve→remote→
//     complete (committed in_progress) saga pattern instead.
//   - The store's conflict handling assumes READ COMMITTED isolation (the default).
type DurableDedup struct {
	Store DurableIdempotencyStore
	Tx    persistence.TxRunner
	// TTL is the record retention; defaults to [DefaultIdempotencyTTL] (~24h).
	TTL time.Duration
	// DisableFingerprint turns OFF the param-fingerprint guard, which is ON by
	// default: with it on, reusing a request_id with a DIFFERENT request body is
	// rejected [ErrIdempotencyFingerprintMismatch] (Stripe-style). Turn it off only
	// when a stable per-request fingerprint is not desired.
	DisableFingerprint bool
	// MaxResponseBytes, when > 0, rejects a response whose marshaled size exceeds it
	// with [ErrIdempotencyResponseTooLarge] (fail loud) — a per-key storage bound.
	// Zero means unlimited (the default).
	MaxResponseBytes int
	// Mode selects the interceptor: DurableModeTransactional (default — claim/handler/
	// complete in one tx, for a LOCAL DB effect) or DurableModeReserve (reserve→remote→
	// complete, for a REMOTE effect). server.New reads it to pick the interceptor.
	Mode DurableDedupMode
}

// Idempotency sentinel errors, mapped to the gRPC/HTTP codes AIP-155 / Stripe /
// OCI use for these cases.
var (
	// ErrIdempotencyInProgress is returned when a duplicate arrives while the
	// original is still in flight — AlreadyExists (HTTP 409), never a re-execution.
	ErrIdempotencyInProgress = status.Error(codes.AlreadyExists, "a request with this request_id is already in progress")
	// ErrIdempotencyFingerprintMismatch is returned when a key is reused with a
	// different request body — InvalidArgument (HTTP 400).
	ErrIdempotencyFingerprintMismatch = status.Error(codes.InvalidArgument, "request_id reused with different request parameters")
	// ErrIdempotencyNonProtoResponse is returned when the durable path cannot
	// serialize the handler's response for replay — a loud Internal error rather
	// than silently degrading to non-durable behavior.
	ErrIdempotencyNonProtoResponse = status.Error(codes.Internal, "durable idempotency requires a protobuf response")
	// ErrIdempotencyRequestIDTooLong is returned when the client's request_id
	// exceeds MaxRequestIDLen — rejected up front as InvalidArgument rather than
	// letting an over-length value hit the store and surface a raw driver error.
	ErrIdempotencyRequestIDTooLong = status.Errorf(codes.InvalidArgument, "request_id exceeds %d characters", MaxRequestIDLen)
	// ErrIdempotencyResponseTooLarge is returned when a response exceeds
	// DurableDedup.MaxResponseBytes — a loud Internal error so the operator either
	// raises the cap or excludes the method.
	ErrIdempotencyResponseTooLarge = status.Error(codes.Internal, "idempotency response exceeds configured MaxResponseBytes")
)

// MaxRequestIDLen bounds a client-supplied request_id, matching the
// idempotency_keys.request_id column width. An over-length id is rejected before
// it reaches the store.
const MaxRequestIDLen = 255

// idempotencyMethodKey carries the resolved idempotency method on ctx.
type idempotencyMethodKey struct{}

// withIdempotencyMethod stashes the resolved gRPC method on ctx. The durable
// interceptors set it before opening any claim/complete transaction so a routing
// TxRunner binds the transaction to the SAME method the store call uses.
func withIdempotencyMethod(ctx context.Context, method string) context.Context {
	return context.WithValue(ctx, idempotencyMethodKey{}, method)
}

// IdempotencyMethodFromContext returns the durable-idempotency method a durable
// interceptor stashed on ctx (and true), or ("", false). A routing TxRunner (e.g.
// servicekit's per-module host holder) reads it to bind the transaction to the same
// backend the store call targets — independent of the transport, so store routing (by
// the key method) and tx routing never diverge.
func IdempotencyMethodFromContext(ctx context.Context) (string, bool) {
	m, ok := ctx.Value(idempotencyMethodKey{}).(string)
	return m, ok && m != ""
}

// DurableDeduplicateUnary returns a gRPC unary interceptor providing durable,
// exactly-once idempotency keyed by (verified tenant, method, request_id).
//
// Fast path (no transaction): a completed record replays the stored response and
// SKIPS the handler entirely. Slow path: open Atomically, claim the key, run the
// handler (its repository write nest-joins this transaction), then complete the
// record in the same commit — so the claim, the effect, and the stored response
// are one atomic unit. A handler error rolls the claim back with the effect
// (errors are never cached, as in F023). Requests without a request_id or with
// validate_only=true pass through untouched.
//
// CONCURRENCY: a genuine concurrent duplicate does NOT get an immediate 409 — its
// claim INSERT blocks on the winner's uncommitted row and then, once the winner
// commits, replays the winner's response (coalesced, exactly-once). The
// [ErrIdempotencyInProgress] (409) result is reserved for observing an already
// COMMITTED in_progress record — the reserve→remote→complete (saga) pattern —
// which the single-transaction handler path never leaves behind.
//
// The key is scoped to the VERIFIED tenant only, not the caller's subject: any
// caller in the tenant that presents a completed request_id replays its response,
// so request_ids MUST be high-entropy (AIP-155 UUIDv4) — a guessable id lets a
// tenant peer replay a response and bypass per-resource checks the handler runs
// internally (method-level authz still applies). With no verified principal the
// tenant is empty and all such callers share one scope; use this behind an
// Authenticator.
func DurableDeduplicateUnary(cfg DurableDedup) grpc.UnaryServerInterceptor {
	ttl := cfg.ttlOrDefault()
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		key, fp, passthrough, gerr := cfg.idemGate(ctx, req, info.FullMethod)
		if gerr != nil {
			return nil, gerr
		}
		if passthrough {
			return handler(ctx, req)
		}
		// Carry the method so a routing TxRunner binds the transaction to the SAME
		// backend the store call (keyed by key.Method) targets — no store/tx divergence.
		ctx = withIdempotencyMethod(ctx, key.Method)

		// Fast path: replay a completed record (zero domain transaction) or reject
		// an in-flight duplicate. A store error here is non-fatal — fall through to
		// the transactional path, which re-derives correctness (FM-02).
		if resp, handled, err := cfg.fastReplay(ctx, key, fp); handled {
			return resp, err
		}

		var resp any
		err := cfg.Tx.Atomically(ctx, func(ctx context.Context) error {
			existing, claimed, cerr := cfg.Store.Claim(ctx, key, fp, ttl)
			if cerr != nil {
				return cerr
			}
			if !claimed {
				switch existing.Status {
				case StatusCompleted:
					r, rerr := replayCompleted(existing, fp)
					if rerr != nil {
						return rerr
					}
					resp = r
					return nil
				default: // in_progress held by a concurrent request
					return ErrIdempotencyInProgress
				}
			}
			// Fresh claim: run the handler; its repository write nest-joins this tx.
			r, herr := handler(ctx, req)
			if herr != nil {
				return herr // rolls back the claim with the effect — errors not cached
			}
			typeName, b, merr := cfg.marshalResponse(r)
			if merr != nil {
				return merr
			}
			if cerr := cfg.Store.Complete(ctx, key, typeName, b); cerr != nil {
				return cerr
			}
			resp = r
			return nil
		})
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
}

// DurableReserveUnary returns a gRPC unary interceptor for the reserve→remote→
// complete SAGA path — the durable idempotency variant for a handler whose effect
// is a REMOTE call rather than a local DB write. Unlike [DurableDeduplicateUnary]
// (which runs the handler INSIDE the claim transaction), it holds NO transaction
// across the handler:
//
//  1. Reserve — claim + COMMIT an in_progress record in its own short transaction,
//     then release it. 2. Remote effect — run the handler OUTSIDE any transaction;
//     the handler performs the side effect and MUST pass the same request_id to the
//     remote system so the remote is idempotent. 3. Complete — transition the record
//     to completed with the stored response in a second short transaction.
//
// It is selected via DurableDedup.Mode == DurableModeReserve. Fast-path replay,
// fingerprinting, TTL, the request_id cap, tenant scoping, and pass-through gates all
// behave exactly as the transactional path.
//
// RETRY / FAILURE SEMANTICS (see spec 048 Increment 2, DB-4):
//   - A duplicate that observes the committed in_progress reservation gets
//     [ErrIdempotencyInProgress] (409) — the reservation is committed, so (unlike the
//     transactional path) the conflict is immediate, not coalesced. A completed
//     duplicate replays verbatim.
//   - Handler (remote) error: the reservation is RELEASED ([DurableIdempotencyStore.Abandon],
//     best-effort) so an immediate retry re-executes — safe because the remote is
//     idempotent by request_id. A release failure leaves the record to expire by TTL.
//   - Handler succeeded but Complete failed (the "remote succeeded, record lost"
//     gap): the reservation is LEFT in_progress and the error propagates. A duplicate
//     within TTL gets 409; after TTL a retry re-executes and the remote dedups.
//     Releasing here is deliberately avoided — it would invite a needless re-run of a
//     succeeded remote effect and drop the 409 guard.
//
// TTL: set it SHORTER than the transactional default if you want fast recovery from
// the Complete gap, but keep it LONGER than the maximum handler/remote latency.
// Abandon (the handler-error release) matches on status=in_progress, not on the claim
// instance, so a TTL shorter than the remote call lets a reservation expire mid-flight,
// be reclaimed by a retry, and then be deleted by the original's late error — forcing a
// harmless (remote-idempotent) re-drive. Keeping TTL > max latency prevents that.
func DurableReserveUnary(cfg DurableDedup) grpc.UnaryServerInterceptor {
	ttl := cfg.ttlOrDefault()
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		key, fp, passthrough, gerr := cfg.idemGate(ctx, req, info.FullMethod)
		if gerr != nil {
			return nil, gerr
		}
		if passthrough {
			return handler(ctx, req)
		}
		// Carry the method so a routing TxRunner binds each short transaction to the
		// SAME backend the store call (keyed by key.Method) targets.
		ctx = withIdempotencyMethod(ctx, key.Method)

		// Fast path: replay a completed record or reject a committed in-flight
		// reservation. A store error here is non-fatal — fall through to Reserve.
		if resp, handled, err := cfg.fastReplay(ctx, key, fp); handled {
			return resp, err
		}

		// (1) Reserve: claim + COMMIT in_progress in its own short transaction — the
		// transaction is released here, NOT held across the remote call below.
		var existing IdempotencyRecord
		var claimed bool
		if rerr := cfg.Tx.Atomically(ctx, func(ctx context.Context) error {
			ex, cl, cerr := cfg.Store.Claim(ctx, key, fp, ttl)
			existing, claimed = ex, cl
			return cerr
		}); rerr != nil {
			return nil, rerr
		}
		if !claimed {
			switch existing.Status {
			case StatusCompleted:
				return replayCompleted(existing, fp)
			default: // committed in_progress reservation held by another request
				return nil, ErrIdempotencyInProgress
			}
		}

		// (2) Remote effect OUTSIDE any transaction. The handler must pass
		// key.RequestID to the remote system so the remote dedups a re-execution.
		resp, herr := handler(ctx, req)
		if herr != nil {
			// Release the reservation so an immediate retry re-executes; safe because
			// the remote is idempotent by request_id. Best-effort: a release failure
			// leaves the reservation to expire by TTL (still correct).
			_ = cfg.abandon(ctx, key)
			return nil, herr
		}
		typeName, b, merr := cfg.marshalResponse(resp)
		if merr != nil {
			// The remote effect already happened but we cannot serialize the response
			// for replay. Release so a retry re-drives (remote dedups) rather than
			// leaving a permanently un-completable reservation.
			_ = cfg.abandon(ctx, key)
			return nil, merr
		}

		// (3) Complete: transition to completed in a second short transaction. On
		// failure the reservation stays in_progress (documented gap) — do NOT release.
		if cerr := cfg.Tx.Atomically(ctx, func(ctx context.Context) error {
			return cfg.Store.Complete(ctx, key, typeName, b)
		}); cerr != nil {
			return nil, cerr
		}
		return resp, nil
	}
}

// ttlOrDefault returns the configured TTL, or DefaultIdempotencyTTL when unset.
func (cfg DurableDedup) ttlOrDefault() time.Duration {
	if cfg.TTL <= 0 {
		return DefaultIdempotencyTTL
	}
	return cfg.TTL
}

// idemGate derives the idempotency key + request fingerprint, and reports whether the
// request bypasses idempotency entirely (no request_id, or validate_only). It is the
// shared front gate of both durable interceptors.
func (cfg DurableDedup) idemGate(ctx context.Context, req any, fullMethod string) (key IdempotencyKey, fp string, passthrough bool, err error) {
	rg, ok := req.(requestIDGetter)
	if !ok {
		return IdempotencyKey{}, "", true, nil
	}
	requestID := rg.GetRequestId()
	if requestID == "" {
		return IdempotencyKey{}, "", true, nil
	}
	if ValidateOnlyFromContext(ctx) {
		return IdempotencyKey{}, "", true, nil
	}
	if len(requestID) > MaxRequestIDLen {
		return IdempotencyKey{}, "", false, ErrIdempotencyRequestIDTooLong
	}
	tenant, _ := VerifiedTenantID(ctx)
	key = IdempotencyKey{Tenant: tenant, Method: fullMethod, RequestID: requestID}
	if !cfg.DisableFingerprint {
		if pm, ok := req.(proto.Message); ok {
			fp = fingerprintRequest(pm)
		}
	}
	return key, fp, false, nil
}

// fastReplay is the shared non-transactional Lookup fast path of both durable
// interceptors: a completed record replays verbatim (with the fingerprint guard); a
// live in_progress record is a 409. handled reports whether the request was resolved
// here (resp/err are then the interceptor's return). A store error is NON-fatal —
// handled is false, so the caller falls through to the transactional/reserve path,
// which re-derives correctness (FM-02).
func (cfg DurableDedup) fastReplay(ctx context.Context, key IdempotencyKey, fp string) (resp any, handled bool, err error) {
	rec, hit, lerr := cfg.Store.Lookup(ctx, key)
	if lerr != nil || !hit {
		return nil, false, nil
	}
	switch rec.Status {
	case StatusCompleted:
		r, rerr := replayCompleted(rec, fp)
		return r, true, rerr
	case StatusInProgress:
		return nil, true, ErrIdempotencyInProgress
	default:
		return nil, false, nil
	}
}

// marshalResponse serializes a handler response for durable storage, enforcing the
// proto requirement (FM-01) and the optional MaxResponseBytes cap.
func (cfg DurableDedup) marshalResponse(resp any) (typeName string, b []byte, err error) {
	pm, ok := resp.(proto.Message)
	if !ok {
		return "", nil, ErrIdempotencyNonProtoResponse
	}
	b, merr := proto.MarshalOptions{Deterministic: true}.Marshal(pm)
	if merr != nil {
		return "", nil, ErrIdempotencyNonProtoResponse
	}
	if cfg.MaxResponseBytes > 0 && len(b) > cfg.MaxResponseBytes {
		return "", nil, ErrIdempotencyResponseTooLarge
	}
	return string(pm.ProtoReflect().Descriptor().FullName()), b, nil
}

// abandon releases an in_progress reservation in its own short transaction (the
// reserve-mode error path). It is best-effort: the caller ignores the error and lets
// TTL expiry reclaim the record if the release does not commit.
func (cfg DurableDedup) abandon(ctx context.Context, key IdempotencyKey) error {
	return cfg.Tx.Atomically(ctx, func(ctx context.Context) error {
		_, err := cfg.Store.Abandon(ctx, key)
		return err
	})
}

// replayCompleted returns the stored response for a completed record after enforcing
// the fingerprint guard: a key reused with a different request body is rejected
// [ErrIdempotencyFingerprintMismatch] rather than replaying a mismatched response.
func replayCompleted(rec IdempotencyRecord, fp string) (any, error) {
	if fp != "" && rec.Fingerprint != "" && fp != rec.Fingerprint {
		return nil, ErrIdempotencyFingerprintMismatch
	}
	return replayResponse(rec)
}

// fingerprintRequest returns the hex SHA-256 of the deterministically-marshaled
// request. Deterministic marshaling makes map ordering stable so the same logical
// request fingerprints identically across retries; the request_id is part of the
// bytes but is constant for a given key, so it does not perturb the comparison.
func fingerprintRequest(m proto.Message) string {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// replayResponse reconstructs the stored proto response so gRPC can marshal it
// back to the client byte-for-byte. The concrete type is resolved from the global
// proto registry by the stored message name (generated .pb.go files register
// their types at init), so the interceptor stays type-agnostic.
func replayResponse(rec IdempotencyRecord) (any, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(rec.ResponseType))
	if err != nil {
		return nil, ErrIdempotencyNonProtoResponse
	}
	msg := mt.New().Interface()
	if err := proto.Unmarshal(rec.Response, msg); err != nil {
		return nil, ErrIdempotencyNonProtoResponse
	}
	return msg, nil
}
