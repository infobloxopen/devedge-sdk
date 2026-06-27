package persistence

import (
	"cmp"
	"context"
	"maps"
	"reflect"
	"slices"
)

// memParticipant is the private capability a [MemoryRepository] exposes to the
// in-memory transaction machinery: take/release its lock, snapshot/restore its
// state, and identify itself on a transaction's participant set. It is
// deliberately unexported — only this package's TxRunner drives it.
type memParticipant interface {
	lockForTx()
	unlockForTx()
	snapshotForTx() any
	restoreForTx(any)
	// lockOrder returns a stable ordering key so a multi-repository transaction
	// always acquires locks in the same order (deadlock avoidance).
	lockOrder() uint64
}

// memTxSet is the in-memory transaction handle carried on ctx. It records every
// repository enrolled in the transaction so each repository's operations can tell
// "I am inside this transaction" (skip re-locking — the runner holds my lock) from
// "a transaction for other repositories is active". A set (not a single owner) is
// what lets a single Atomically span a parent and a child repository atomically —
// the F030 AC-1 shape.
type memTxSet struct {
	members map[memParticipant]struct{}
}

func (s *memTxSet) has(p memParticipant) bool {
	_, ok := s.members[p]
	return ok
}

// memTxSetFromContext returns the active in-memory transaction set on ctx, if any.
func memTxSetFromContext(ctx context.Context) (*memTxSet, bool) {
	if h, ok := TxFromContext(ctx); ok {
		s, ok := h.(*memTxSet)
		return s, ok
	}
	return nil, false
}

// --- memParticipant implementation for MemoryRepository ---

func (r *MemoryRepository[T, K]) lockForTx()         { r.mu.Lock() }
func (r *MemoryRepository[T, K]) unlockForTx()       { r.mu.Unlock() }
func (r *MemoryRepository[T, K]) snapshotForTx() any { return r.snapshotState() }
func (r *MemoryRepository[T, K]) restoreForTx(s any) { r.restore(s.(snapshot[T, K])) }
func (r *MemoryRepository[T, K]) lockOrder() uint64  { return r.id }

// snapshot is a point-in-time copy of the mutable state, restored on rollback.
type snapshot[T any, K comparable] struct {
	items   map[K]T
	etags   map[K]string
	keys    []K
	deleted map[K]bool
}

// cloneEntity returns a snapshot-safe copy of v. When T is a POINTER type (the
// common case — aggregates are stored as *Proto / *Struct), a shallow map copy
// would share the pointed-to struct between the snapshot and the live map, so a
// rollback could not undo an in-place mutation of a Get-returned entity (the caller
// does `e, _ := repo.Get(...); e.Field = x; repo.Update(...)` — a single struct
// mutated in place). cloneEntity copies the pointee so the snapshot is isolated:
// restoring it on rollback truly reverts the field. A nil pointer or a non-pointer
// T (value semantics already isolate it) is returned unchanged. This is the same
// deep-copy MemoryOutboxStore.snapshotForTx already does for *OutboxRecord.
func cloneEntity[T any](v T) T {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return v
	}
	cp := reflect.New(rv.Elem().Type())
	cp.Elem().Set(rv.Elem())
	return cp.Interface().(T)
}

func (r *MemoryRepository[T, K]) snapshotState() snapshot[T, K] {
	items := make(map[K]T, len(r.items))
	for k, v := range r.items {
		items[k] = cloneEntity(v)
	}
	etags := make(map[K]string, len(r.etags))
	maps.Copy(etags, r.etags)
	deleted := make(map[K]bool, len(r.deleted))
	maps.Copy(deleted, r.deleted)
	keys := slices.Clone(r.keys)
	return snapshot[T, K]{items: items, etags: etags, keys: keys, deleted: deleted}
}

func (r *MemoryRepository[T, K]) restore(s snapshot[T, K]) {
	r.items = s.items
	r.etags = s.etags
	r.keys = s.keys
	r.deleted = s.deleted
}

// inThisTx reports whether ctx carries a transaction this repository is enrolled
// in. When true the caller must NOT take r.mu — the runner already holds the write
// lock for the whole transaction, and r.mu is not reentrant.
func (r *MemoryRepository[T, K]) inThisTx(ctx context.Context) bool {
	if s, ok := memTxSetFromContext(ctx); ok {
		return s.has(r)
	}
	return false
}

// MemoryTxRunner is the in-memory [TxRunner]. It coordinates one or more
// [MemoryRepository] instances so a single Atomically can span them atomically —
// the canonical "load the parent, check it, write the child" shape (F030 AC-1),
// where parent and child are separate repositories.
//
// On Atomically it takes every participant's write lock (in a stable order),
// snapshots each, enrolls a transaction set on ctx, then runs fn; repository
// operations issued through the enrolled repositories inside fn detect the
// transaction and mutate the live maps without re-locking. On success the
// mutations are kept; on an error (or panic) every snapshot is restored before the
// locks are released, discarding the work as one unit.
//
// Because the write locks are held across fn, a concurrent reader of any
// participant blocks until the transaction completes and therefore never observes
// partial state — it sees either the pre-transaction snapshot (rollback) or the
// committed result.
//
// Nested Atomically joins the outer transaction: when ctx already carries a
// transaction this runner's participants are part of, fn runs against the same
// locked state with no second lock, snapshot, or commit (a no-op begin).
type MemoryTxRunner struct {
	participants []memParticipant
}

// MemoryRepositoryFor is the interface a value must satisfy to enroll in a
// [MemoryTxRunner]; every *[MemoryRepository] does. It is unexported in spirit
// (only the package's repositories implement the private methods), exported only
// so callers can name it in a slice.
type MemoryRepositoryFor interface {
	memParticipant
}

// NewMemoryTxRunner returns an in-memory TxRunner coordinating the given
// repositories. Pass every repository that may be written inside one Atomically
// (e.g. the parent and child repositories of an aggregate). A write through a
// repository NOT passed here will not see the transaction — see the failure-mode
// note in concepts/transactions.md.
func NewMemoryTxRunner(repos ...MemoryRepositoryFor) *MemoryTxRunner {
	// De-duplicate: the same repository passed twice would otherwise be locked
	// twice in Atomically — a self-deadlock on the non-reentrant write lock.
	seen := make(map[memParticipant]struct{}, len(repos))
	ps := make([]memParticipant, 0, len(repos))
	for _, r := range repos {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		ps = append(ps, r)
	}
	// Stable lock order across all transactions of this runner (deadlock avoidance
	// if two runners ever overlap on a shared repository).
	slices.SortFunc(ps, func(a, b memParticipant) int {
		return cmp.Compare(a.lockOrder(), b.lockOrder())
	})
	return &MemoryTxRunner{participants: ps}
}

// WithParticipants returns a NEW runner that coordinates this runner's
// participants PLUS extra, de-duplicated and re-sorted into the stable lock order.
// It lets a caller enroll an additional repository in the same atomic unit without
// reconstructing the original participant list — used by the events dispatcher to
// enlist its idempotency-marker store in the handler's transaction so the marker
// commits/rolls back atomically with the handler's aggregate write. The receiver is
// unchanged.
func (m *MemoryTxRunner) WithParticipants(extra ...MemoryRepositoryFor) *MemoryTxRunner {
	combined := make([]MemoryRepositoryFor, 0, len(m.participants)+len(extra))
	for _, p := range m.participants {
		combined = append(combined, p)
	}
	combined = append(combined, extra...)
	return NewMemoryTxRunner(combined...)
}

// Atomically implements [TxRunner].
func (m *MemoryTxRunner) Atomically(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	// Nested: if any participant is already enrolled in a transaction on ctx, join
	// it (the outer call owns the locks, snapshots, and commit/rollback decision).
	if s, ok := memTxSetFromContext(ctx); ok {
		if slices.ContainsFunc(m.participants, s.has) {
			return fn(ctx)
		}
	}

	for _, p := range m.participants {
		p.lockForTx()
	}
	snaps := make([]any, len(m.participants))
	for i, p := range m.participants {
		snaps[i] = p.snapshotForTx()
	}
	committed := false
	defer func() {
		if !committed {
			for i, p := range m.participants {
				p.restoreForTx(snaps[i])
			}
		}
		for i := len(m.participants) - 1; i >= 0; i-- {
			m.participants[i].unlockForTx()
		}
	}()

	set := &memTxSet{members: make(map[memParticipant]struct{}, len(m.participants))}
	for _, p := range m.participants {
		set.members[p] = struct{}{}
	}
	if ferr := fn(WithTx(ctx, set)); ferr != nil {
		return ferr // deferred restore rolls back
	}
	committed = true
	return nil
}

// compile-time check: MemoryTxRunner is a TxRunner.
var _ TxRunner = (*MemoryTxRunner)(nil)
