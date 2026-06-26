package persistence

import (
	"context"
	"errors"
)

// ErrNoTransaction is returned by [RequireTx] when ctx is not enrolled in a
// transaction. It lets code that must only run inside [TxRunner.Atomically] fail
// loudly rather than silently writing outside any transaction — the F030 "tx not
// propagated" failure mode (an un-enrolled write looks atomic but is not).
var ErrNoTransaction = errors.New("persistence: no transaction on context")

// TxRunner runs a function inside a single backend transaction. Repositories
// used inside fn are transaction-bound: the work commits when fn returns nil and
// rolls back when fn returns an error (or panics).
//
// Propagation is context-based. Atomically stashes a backend transaction handle
// on ctx (via [WithTx]); a tx-aware repository reads it (via [TxFromContext]) and
// binds its writes to that transaction for the duration of fn. This keeps the
// clean-core rule intact: the seam carries the handle as an opaque any, so package
// persistence never imports an ORM, driver, or policy engine. The concrete handle
// type is known only to the backend's TxRunner and its generated repositories.
//
// Nested Atomically calls join the outer transaction: an implementation that finds
// a handle already on ctx must run fn against it without opening (or committing)
// a second transaction.
type TxRunner interface {
	Atomically(ctx context.Context, fn func(ctx context.Context) error) error
}

// txKey is the unexported context key under which a backend transaction handle is
// carried. The unexported type makes the key un-forgeable from outside this
// package, so the only way to put a handle on ctx is via [WithTx].
type txKey struct{}

// WithTx returns a copy of ctx carrying the backend transaction handle. A
// TxRunner.Atomically implementation calls it before invoking fn so the tx-aware
// repositories created against the same backend can discover the transaction.
//
// The handle is opaque to this package (typed any) to keep the core free of any
// ORM/driver import; the backend that produced it type-asserts it back.
func WithTx(ctx context.Context, handle any) context.Context {
	return context.WithValue(ctx, txKey{}, handle)
}

// TxFromContext returns the backend transaction handle carried on ctx and true
// when [Atomically] has enrolled the context, or (nil, false) otherwise. A
// tx-aware repository calls it on every operation to decide whether to bind to the
// transaction or fall back to its constructor-time client.
func TxFromContext(ctx context.Context) (any, bool) {
	h := ctx.Value(txKey{})
	return h, h != nil
}

// RequireTx returns nil when ctx is enrolled in a transaction and
// [ErrNoTransaction] otherwise. It is the failure-mode guard for "tx not
// propagated": a write path that must be atomic can call it first so a caller who
// forgot to wrap the work in [TxRunner.Atomically] gets a clear error instead of a
// silent non-transactional write.
//
// Note the limitation: this guards only callers that opt in by calling it. It
// cannot prove that a repository which did NOT consult ctx (a non-tx-aware
// adapter) participated — that is why the tx-aware adapters are the generated
// default. See concepts/transactions.md.
func RequireTx(ctx context.Context) error {
	if _, ok := TxFromContext(ctx); ok {
		return nil
	}
	return ErrNoTransaction
}
