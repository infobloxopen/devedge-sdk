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

// cloneEntity returns a snapshot-safe DEEP copy of v. Aggregates are stored as
// *Proto / *Struct whose fields routinely include nested slices, maps, and pointers
// (e.g. an order root with `Items []*item`). A shallow copy would share that nested
// data between the snapshot and the live map, so a rollback could not undo an
// in-place mutation reached THROUGH a Get-returned entity (the caller does
// `e, _ := repo.Get(...); e.Items[0].SKU = x` or `e.Tags = append(e.Tags, ...)`
// — mutating nested state in place). cloneEntity recursively copies the reachable
// exported value so the snapshot is isolated: restoring it on rollback truly reverts
// every level, not just the top.
//
// Unexported fields (e.g. a generated proto message's internal state/sizeCache/
// unknownFields) are copied by value at the struct level but not deep-copied — they
// are runtime bookkeeping a handler never mutates in place, so a shared reference
// there is harmless (and reflection cannot deep-copy them without unsafe). A value
// type (or nil pointer) is returned unchanged: value semantics already isolate the
// top level, and its nested references are still shared only as far as a value-typed
// T would share them, which is the pre-existing, non-pointer contract.
func cloneEntity[T any](v T) T {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return v
	}
	if rv.Kind() != reflect.Pointer && rv.Kind() != reflect.Slice && rv.Kind() != reflect.Map {
		// Top-level value semantics already isolate v from the live map; deep-copying a
		// value-typed T would be a behavior change (it was always shared at nested
		// levels). Keep the historical non-pointer contract.
		return v
	}
	return deepCopyValue(rv, map[uintptr]reflect.Value{}).Interface().(T)
}

// deepCopyValue returns a recursively isolated copy of rv: pointers, slices, maps,
// arrays, structs, and the concrete value held by a non-nil interface are
// reconstructed so no nested reference is shared with the source. Unexported
// (un-settable) struct fields are left as the value-copy made by the enclosing
// Set — they are not deep-copied (reflection cannot set them) but are not mutated
// in place by handlers either. Scalars and nil interfaces fall through as a plain
// value copy.
//
// seen maps an already-copied pointer's address to its copy so a cyclic or
// shared-pointer graph (a DAG, or a struct that points back at itself) is copied
// once and re-aliased rather than recursed forever — turning a would-be infinite
// recursion into a bounded, structure-preserving copy.
func deepCopyValue(rv reflect.Value, seen map[uintptr]reflect.Value) reflect.Value {
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return rv
		}
		addr := rv.Pointer()
		if cp, ok := seen[addr]; ok {
			return cp // already copying/copied this pointer — re-alias (breaks cycles)
		}
		cp := reflect.New(rv.Elem().Type())
		seen[addr] = cp // record BEFORE recursing so a back-reference resolves
		cp.Elem().Set(deepCopyValue(rv.Elem(), seen))
		return cp
	case reflect.Slice:
		if rv.IsNil() {
			return rv
		}
		cp := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Cap())
		for i := 0; i < rv.Len(); i++ {
			cp.Index(i).Set(deepCopyValue(rv.Index(i), seen))
		}
		return cp
	case reflect.Map:
		if rv.IsNil() {
			return rv
		}
		cp := reflect.MakeMapWithSize(rv.Type(), rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			cp.SetMapIndex(deepCopyValue(iter.Key(), seen), deepCopyValue(iter.Value(), seen))
		}
		return cp
	case reflect.Array:
		cp := reflect.New(rv.Type()).Elem()
		for i := 0; i < rv.Len(); i++ {
			cp.Index(i).Set(deepCopyValue(rv.Index(i), seen))
		}
		return cp
	case reflect.Struct:
		cp := reflect.New(rv.Type()).Elem()
		cp.Set(rv) // value-copy every field, including unexported ones
		for i := 0; i < rv.NumField(); i++ {
			f := cp.Field(i)
			if !f.CanSet() {
				continue // unexported: keep the value-copy made above
			}
			f.Set(deepCopyValue(rv.Field(i), seen))
		}
		return cp
	case reflect.Interface:
		// An interface field (e.g. a proto `oneof` wrapper, *anypb.Any, or any
		// any-typed field) holds a concrete dynamic value — frequently a pointer to a
		// mutable struct. Returning rv unchanged would SHARE that dynamic value with
		// the source, so an in-place mutation reached through the interface would
		// survive a rollback — the very leak the deep copy exists to prevent. Unwrap
		// the concrete value, deep-copy it, and re-wrap it preserving the static
		// interface type. A nil interface (or one holding an un-copyable kind) falls
		// through as a plain value copy.
		if rv.IsNil() {
			return rv
		}
		inner := deepCopyValue(rv.Elem(), seen)
		// Re-wrap the (possibly copied) concrete value back into the interface type so
		// the field keeps its declared interface type rather than the concrete type.
		out := reflect.New(rv.Type()).Elem()
		if inner.Type().AssignableTo(rv.Type()) {
			out.Set(inner)
			return out
		}
		return rv // concrete value not assignable back (shouldn't happen) — value copy
	default:
		return rv
	}
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
