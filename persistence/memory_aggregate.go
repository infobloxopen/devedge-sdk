package persistence

import "context"

// AggregateSpec wires the aggregate-shape knowledge a backend-neutral
// AggregateRepository needs but cannot infer from a flat repository: how to load
// the root, eager-load its members, and persist member mutations. The root
// Repository (Root) supplies the root row's CRUD + etag, and TxRunner makes the
// whole Save atomic. This is the "bespoke graph assembly" the memory backend
// needs (it has no graph of its own); the ent backend gets the same shape from a
// generated graph-load primitive.
//
// Save semantics are MEMBER-MUTATION TRACKING (D-3): SaveMembers compares the
// incoming root's members to what is stored and applies the adds/removes/changes
// itself (so it can drive cascade/orphan correctly), reporting via its bool
// return whether anything changed — which is what triggers the single root etag
// bump (D-5). Save never blindly full-replaces.
type AggregateSpec[Root any, ID comparable] struct {
	// Tx runs Save's work in one transaction. Required.
	Tx TxRunner
	// RootRepo is the root row's repository (Load reads it; Save bumps its etag).
	// Required.
	RootRepo Repository[Root, ID]
	// KeyOf extracts the root id from a root value. Required.
	KeyOf func(Root) ID
	// EtagOf extracts the root's etag (the aggregate version) from a root value.
	// Optional: when set, Save enforces an OPTIMISTIC-CONCURRENCY precondition —
	// the incoming root's etag must still match the stored root's etag, else Save
	// fails with [ErrPreconditionFailed] (etag-as-aggregate-version, D-5/AC-3). The
	// stored etag is read from LoadEtag when provided, else from EtagOf(storedRoot)
	// (which suits a backend that projects the etag into the root struct, e.g. ent).
	// When EtagOf is nil the precondition is skipped.
	EtagOf func(Root) string
	// LoadEtag returns the CURRENTLY stored root etag for id. Optional: used for the
	// precondition's stored side when a backend keeps the etag out-of-band rather
	// than in the root struct (e.g. the in-memory repository's etag map). When nil,
	// the stored etag comes from EtagOf(storedRoot).
	LoadEtag func(ctx context.Context, id ID) (string, error)
	// LoadMembers eager-loads the owned members onto root and returns it. Called by
	// Load after the root row is fetched. Required.
	LoadMembers func(ctx context.Context, root Root) (Root, error)
	// SaveMembers applies member mutations (add/remove/change) for root against the
	// stored cluster and returns whether any member changed (drives the etag bump).
	// Runs inside the Save transaction. Required.
	SaveMembers func(ctx context.Context, root Root) (changed bool, err error)
}

// GenericAggregateRepository is the backend-neutral (ent/GORM/memory)
// [AggregateRepository]. It composes a root [Repository] (for the root row + its
// etag), a member-mutation SaveMembers closure, and a graph-assembly LoadMembers
// closure via [AggregateSpec], and runs Save inside the spec's [TxRunner] so root
// + members commit or roll back as one. The backend is supplied entirely through
// the spec's closures + [TxRunner], so the SAME implementation serves the
// in-memory, ent, and GORM backends — only the wiring (which TxRunner, which
// Repository, which Load/SaveMembers closures) differs.
type GenericAggregateRepository[Root any, ID comparable] struct {
	spec AggregateSpec[Root, ID]
}

// NewGenericAggregateRepository returns a GenericAggregateRepository over spec.
func NewGenericAggregateRepository[Root any, ID comparable](spec AggregateSpec[Root, ID]) *GenericAggregateRepository[Root, ID] {
	return &GenericAggregateRepository[Root, ID]{spec: spec}
}

// MemoryAggregateRepository is a back-compat alias for [GenericAggregateRepository]
// (the type was renamed when the GORM backend began reusing it — it was never
// memory-specific). Prefer GenericAggregateRepository in new code.
type MemoryAggregateRepository[Root any, ID comparable] = GenericAggregateRepository[Root, ID]

// NewMemoryAggregateRepository is a back-compat shim for
// [NewGenericAggregateRepository] (a generic function cannot be a var alias).
// Prefer NewGenericAggregateRepository in new code.
func NewMemoryAggregateRepository[Root any, ID comparable](spec AggregateSpec[Root, ID]) *GenericAggregateRepository[Root, ID] {
	return NewGenericAggregateRepository(spec)
}

// Load implements [AggregateRepository]: fetch the root row, then eager-load its
// owned members.
func (r *GenericAggregateRepository[Root, ID]) Load(ctx context.Context, id ID) (Root, error) {
	root, err := r.spec.RootRepo.Get(ctx, id)
	if err != nil {
		var zero Root
		return zero, err
	}
	return r.spec.LoadMembers(ctx, root)
}

// Save implements [AggregateRepository]: run the root's Validate(ctx) invariant
// hook (D-7), then in one transaction RE-VALIDATE the optimistic-concurrency
// precondition, apply member mutations and, on any member change, bump the root
// etag EXACTLY once (D-5). A stale root etag surfaces as [ErrPreconditionFailed].
func (r *GenericAggregateRepository[Root, ID]) Save(ctx context.Context, root Root) (Root, error) {
	// Domain invariant hook (pre-persist), by convention.
	if err := ValidateAggregate(ctx, root); err != nil {
		var zero Root
		return zero, err
	}
	var saved Root
	err := r.spec.Tx.Atomically(ctx, func(ctx context.Context) error {
		// etag-as-aggregate-version precondition (D-5/AC-3): the version the caller
		// loaded must still be current, else another writer changed the cluster. This
		// MUST run INSIDE the transaction so the check and the subsequent member
		// writes + etag bump are one atomic compare-and-set against the serialized
		// critical section — otherwise two Saves that both read etag=vN before
		// entering the tx would both pass the precondition and both commit (a lost
		// update, defeating optimistic concurrency). The in-memory runner holds the
		// write lock across the whole tx and an ent tx provides isolation, so the
		// stored read here observes the latest committed/locked version; LoadEtag and
		// RootRepo.Get are tx-aware (see GetETagForKeyTx) and do not re-enter the lock.
		if err := r.checkPrecondition(ctx, root); err != nil {
			return err
		}
		changed, err := r.spec.SaveMembers(ctx, root)
		if err != nil {
			return err
		}
		// etag-as-aggregate-version (D-5): bump the root etag once on any member
		// change via a single explicit root Update. When no member changed we skip
		// the touch, so a pure read-modify-nothing Save does not churn the etag,
		// and a direct root field change (a real Update) is not double-bumped here.
		if changed {
			updated, uerr := r.spec.RootRepo.Update(ctx, r.spec.KeyOf(root), root)
			if uerr != nil {
				return uerr
			}
			saved = updated
			return nil
		}
		saved = root
		return nil
	})
	if err != nil {
		var zero Root
		return zero, err
	}
	return saved, nil
}

// checkPrecondition enforces the etag-as-aggregate-version optimistic-concurrency
// precondition: the etag the caller loaded (EtagOf(root)) must still match the
// CURRENTLY stored root etag, else another writer has changed the cluster and the
// Save fails with [ErrPreconditionFailed]. It is called from INSIDE the Save
// transaction so it serializes with concurrent Saves (compare-and-set). When
// EtagOf is nil the precondition is skipped. The stored side comes from LoadEtag
// when provided (a backend keeping the etag out-of-band, e.g. the memory repo's
// etag map — which MUST be read tx-aware) else from EtagOf(RootRepo.Get) (a
// backend that projects the etag into the root struct, e.g. ent).
func (r *GenericAggregateRepository[Root, ID]) checkPrecondition(ctx context.Context, root Root) error {
	if r.spec.EtagOf == nil {
		return nil
	}
	want := r.spec.EtagOf(root)
	var storedEtag string
	if r.spec.LoadEtag != nil {
		le, lerr := r.spec.LoadEtag(ctx, r.spec.KeyOf(root))
		if lerr != nil {
			return lerr
		}
		storedEtag = le
	} else {
		stored, gerr := r.spec.RootRepo.Get(ctx, r.spec.KeyOf(root))
		if gerr != nil {
			return gerr
		}
		storedEtag = r.spec.EtagOf(stored)
	}
	if want != "" && storedEtag != "" && want != storedEtag {
		return ErrPreconditionFailed
	}
	return nil
}

// compile-time check.
var _ AggregateRepository[struct{}, string] = (*GenericAggregateRepository[struct{}, string])(nil)
