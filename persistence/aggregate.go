package persistence

import "context"

// AggregateRepository loads and saves a DDD AGGREGATE — a root resource together
// with the members it owns (its containment cluster) — as one consistency unit.
// Unlike the per-table [Repository] seam, an AggregateRepository treats the
// cluster atomically:
//
//   - Load eager-loads the root and its owned members in one read.
//   - Save persists the whole cluster in ONE transaction (via [TxRunner]),
//     tracking member mutations (added/removed/changed members), running the
//     root's optional Validate(ctx) invariant hook, and bumping the root's etag
//     exactly once on any member change (etag-as-aggregate-version). A stale root
//     etag (If-Match mismatch) fails with [ErrPreconditionFailed].
//
// The seam stays clean-core: package persistence imports no ORM/driver. One
// backend-neutral implementation ([GenericAggregateRepository]) serves the ent,
// GORM, and in-memory backends — each supplies its own graph-load + per-root saver
// closures and [TxRunner] through an [AggregateSpec].
type AggregateRepository[Root any, ID comparable] interface {
	// Load returns the aggregate root identified by id with its owned members
	// populated. Returns [ErrNotFound] when no root has that id.
	Load(ctx context.Context, id ID) (Root, error)
	// Save persists the aggregate cluster atomically and returns the saved root
	// (with a refreshed etag when members changed). Returns [ErrPreconditionFailed]
	// on a stale root etag and propagates a root Validate(ctx) error.
	Save(ctx context.Context, root Root) (Root, error)
}

// AggregateValidator is the by-convention domain-invariant hook (D-7). A root
// type that implements it has Validate(ctx) called by Save BEFORE any persist, so
// a violated invariant ("a group must keep ≥1 admin", "no item once SHIPPED")
// rejects the Save with the returned error (the ErrorMapper maps it to a gRPC
// code). Implement it in the regen-safe domain-behavior file beside the generated
// aggregate code. A root that does not implement it simply skips the check.
type AggregateValidator interface {
	Validate(ctx context.Context) error
}

// ValidateAggregate runs the root's Validate(ctx) invariant hook when the root
// implements [AggregateValidator], and is a no-op otherwise. Save implementations
// call it pre-persist (D-7). It is exported so a generated/hand-written ent
// aggregate Save can reuse the exact same convention as the memory backend.
func ValidateAggregate(ctx context.Context, root any) error {
	if v, ok := root.(AggregateValidator); ok {
		return v.Validate(ctx)
	}
	return nil
}
