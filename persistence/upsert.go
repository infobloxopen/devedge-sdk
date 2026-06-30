package persistence

import (
	"context"
	"errors"
	"fmt"
)

// NaturalKey is a resource's business key — the stable, human-meaningful identity
// used for import/export and cross-environment matching. Storage keys (K, usually
// a generated UUID) differ across environments, so an export taken from staging
// cannot be re-imported into production by storage key; the natural key is what
// survives the round trip ("account/prod-us-east", an FQDN, a SKU). The seam is
// the load-bearing primitive for P4: import matches on it, references resolve to
// it, and Upsert keys create-or-update on it.
//
// A NaturalKey is a single comparable string so it can key maps and back a unique
// index directly. Composite business keys (e.g. (parent, name)) encode to one
// string deterministically in the caller's key function — the SDK does not impose
// an encoding. The key is tenant-scoped by the repository exactly as every other
// operation is: the same NaturalKey under two tenants is two distinct resources,
// so a leaky cross-tenant match is structurally impossible.
type NaturalKey string

// EntityState is the lifecycle state in which a natural-key lookup found a key.
// It is what lets Upsert distinguish create from update from resurrect.
type EntityState int

const (
	// StateAbsent: no row (live or soft-deleted) holds the natural key.
	StateAbsent EntityState = iota
	// StateLive: a live row holds the natural key — Upsert updates it.
	StateLive
	// StateDeleted: a soft-deleted row holds the natural key — Upsert resurrects
	// it (Undelete) and then writes the imported values. This is the recycle-bin /
	// "new key encountered that used to exist" case, enabled by the dialect-aware
	// partial-unique soft-delete that keeps the key free among live rows while a
	// tombstone retains it.
	StateDeleted
)

// LookupFunc resolves a natural key to its storage key and lifecycle state within
// the caller's tenant (taken from ctx, exactly as the repository scopes every
// other call). It MUST consider soft-deleted rows so Upsert can resurrect: a key
// held only by a tombstone returns (key, StateDeleted, nil); a free key returns
// (zeroK, StateAbsent, nil).
//
// Returning the lookup as an explicit function (rather than a generated query) is
// the same opt-in posture the change feed takes with ChangeEmitting: the SDK
// supplies the mechanism (create/update/resurrect orchestration), the service
// supplies the one thing only it knows — how to find a row by its business key.
// A generated implementation (from a (field.v1.opts).natural_key annotation over
// the existing unique index) is the planned ergonomic follow-up; it would satisfy
// exactly this function shape.
type LookupFunc[K comparable] func(ctx context.Context, nk NaturalKey) (key K, state EntityState, err error)

// UpsertRepository extends Repository with natural-key create-or-update — the P4
// seam. It is what an import tool, a backup/restore, or any idempotent
// reconciler writes against: identity is the business key, not a storage id, so
// the same call is correct on first import (create), re-import (update), and
// re-import after deletion (resurrect).
type UpsertRepository[T any, K comparable] interface {
	Repository[T, K]

	// LookupByNaturalKey returns the LIVE entity holding nk, or ErrNotFound when
	// no live row holds it (whether absent or soft-deleted). Tenant-scoped.
	LookupByNaturalKey(ctx context.Context, nk NaturalKey) (T, error)

	// Upsert creates-or-updates entity keyed by its natural key, atomically:
	//   - absent key            -> Create        (created = true)
	//   - live row holds it     -> Update        (created = false)
	//   - soft-deleted holds it -> Undelete+Update i.e. resurrect (created = false)
	// Upsert is idempotent under retry: a second identical call updates rather than
	// duplicating, because the match is on the natural key, not insertion.
	Upsert(ctx context.Context, entity T) (result T, created bool, err error)
}

// ErrCycle is returned by TopoSort when the dependency graph has a cycle, so a
// safe parents-before-children import order does not exist.
var ErrCycle = errors.New("persistence: dependency cycle among natural keys")

// upserter is the portable, backend-neutral UpsertRepository: it composes the
// base Repository's Create/Update/Undelete with a caller-supplied lookup, opening
// a transaction per upsert so the resurrect path (Undelete then Update) is atomic.
// It depends on nothing beyond the Repository seam, so it lives in the root module
// and works over any backend (in-memory, gorm, ent) without an ORM dependency. A
// DB-native batched INSERT ... ON CONFLICT fast-path belongs in an adapter
// (persistence/gormtx) behind this same interface; see BatchUpsert.
type upserter[T any, K comparable] struct {
	base      Repository[T, K]
	tx        TxRunner
	keyOf     func(T) NaturalKey
	lookup    LookupFunc[K]
	assignKey func(T, K) T
}

// UpsertOption configures an UpsertRepository.
type UpsertOption[T any, K comparable] func(*upserter[T, K])

// WithKeyAssignment supplies a function that stamps the matched row's storage key
// onto the imported entity before an update-by-natural-key. Use it whenever the
// entity value embeds its own storage key (an ID field): a re-import from another
// environment carries that environment's id (or none), and a natural-key update
// must preserve the EXISTING row's immutable storage identity rather than overwrite
// it. A UUID-PK store (ent) already ignores the import's id, so this is a no-op
// there; a store that persists the value verbatim needs it to stay consistent.
func WithKeyAssignment[T any, K comparable](fn func(entity T, key K) T) UpsertOption[T, K] {
	return func(u *upserter[T, K]) { u.assignKey = fn }
}

// NewUpsert builds an UpsertRepository over base. keyOf extracts an entity's
// natural key; lookup resolves a natural key to a storage key + lifecycle state
// (including soft-deleted rows). tx makes the resurrect path (Undelete+Update)
// atomic and joins any outer transaction, so an Upsert composed under a
// ChangeEmitting feed still commits once. See [WithKeyAssignment] for stores whose
// entity value embeds its storage key.
func NewUpsert[T any, K comparable](
	base Repository[T, K],
	tx TxRunner,
	keyOf func(T) NaturalKey,
	lookup LookupFunc[K],
	opts ...UpsertOption[T, K],
) UpsertRepository[T, K] {
	u := &upserter[T, K]{base: base, tx: tx, keyOf: keyOf, lookup: lookup}
	for _, o := range opts {
		o(u)
	}
	return u
}

func (u *upserter[T, K]) Get(ctx context.Context, key K) (T, error) { return u.base.Get(ctx, key) }
func (u *upserter[T, K]) List(ctx context.Context, opts ListOptions) ([]T, string, error) {
	return u.base.List(ctx, opts)
}
func (u *upserter[T, K]) Create(ctx context.Context, e T) (T, error) { return u.base.Create(ctx, e) }
func (u *upserter[T, K]) Update(ctx context.Context, key K, e T, fm ...string) (T, error) {
	return u.base.Update(ctx, key, e, fm...)
}
func (u *upserter[T, K]) Delete(ctx context.Context, key K) error { return u.base.Delete(ctx, key) }
func (u *upserter[T, K]) Undelete(ctx context.Context, key K) (T, error) {
	return u.base.Undelete(ctx, key)
}

func (u *upserter[T, K]) LookupByNaturalKey(ctx context.Context, nk NaturalKey) (T, error) {
	var zero T
	key, state, err := u.lookup(ctx, nk)
	if err != nil {
		return zero, err
	}
	if state != StateLive {
		return zero, ErrNotFound
	}
	return u.base.Get(ctx, key)
}

func (u *upserter[T, K]) Upsert(ctx context.Context, entity T) (T, bool, error) {
	var (
		out     T
		created bool
	)
	nk := u.keyOf(entity)
	err := u.tx.Atomically(ctx, func(ctx context.Context) error {
		key, state, err := u.lookup(ctx, nk)
		if err != nil {
			return err
		}
		switch state {
		case StateAbsent:
			out, err = u.base.Create(ctx, entity)
			created = err == nil
			return err
		case StateDeleted:
			// Resurrect: clear the tombstone, then overwrite with imported values.
			if _, err = u.base.Undelete(ctx, key); err != nil {
				return err
			}
			fallthrough
		case StateLive:
			// Preserve the existing row's immutable storage identity over the
			// import's (a re-import carries a foreign or no storage key).
			if u.assignKey != nil {
				entity = u.assignKey(entity, key)
			}
			out, err = u.base.Update(ctx, key, entity)
			return err
		default:
			return fmt.Errorf("persistence: upsert %q: unknown entity state %d", nk, state)
		}
	})
	return out, created, err
}

var _ UpsertRepository[any, string] = (*upserter[any, string])(nil)

// RowError is one row's failure in a BatchUpsert, carrying enough to build the
// per-row error log a bulk import reports: the row's position in the input, its
// natural key, and the cause. A failed row does not abort the import.
type RowError struct {
	Index int
	Key   NaturalKey
	Err   error
}

func (e RowError) Error() string {
	return fmt.Sprintf("row %d (%q): %v", e.Index, e.Key, e.Err)
}
func (e RowError) Unwrap() error { return e.Err }

// BatchUpsertReport summarises a BatchUpsert: how many rows were created, updated,
// and failed, with the per-row errors for the failures (the import error log).
type BatchUpsertReport struct {
	Created int
	Updated int
	Failed  int
	Errors  []RowError
}

// BatchUpsertOptions tunes a BatchUpsert.
type BatchUpsertOptions struct {
	// ChunkSize bounds how many rows are processed before Progress is reported and
	// (for an LRO-driven import) a checkpoint can be taken. <= 0 means one chunk.
	ChunkSize int

	// StopOnError aborts the batch at the first row error instead of the default
	// continue-on-error (one bad row must not fail a 100k-row import). When true the
	// report still lists the row that stopped it.
	StopOnError bool

	// Progress, if set, is called after each chunk with (rows attempted so far,
	// total rows) so a resumable importer can persist a checkpoint per chunk.
	Progress func(done, total int)
}

// BatchUpsert applies Upsert to each entity, in order, collecting a per-row error
// log and continuing past failures by default. It is the dependency-light
// dev-default: it batches progress/checkpointing by chunk but upserts row-by-row,
// which is correct on every backend. A DB-native multi-row INSERT ... ON CONFLICT
// (natural_key) DO UPDATE fast-path — the enterprise import throughput path — is a
// follow-up that an adapter (persistence/gormtx, which already uses
// clause.OnConflict for the outbox/fence/barrier) can add behind this same
// interface; the import tool above it does not change.
//
// Callers that need cross-type reference ordering should TopoSort the input first
// (parents before children) and may call BatchUpsert once per dependency level.
func BatchUpsert[T any, K comparable](
	ctx context.Context,
	repo UpsertRepository[T, K],
	entities []T,
	keyOf func(T) NaturalKey,
	opts BatchUpsertOptions,
) (BatchUpsertReport, error) {
	report := BatchUpsertReport{}
	total := len(entities)
	chunk := opts.ChunkSize
	if chunk <= 0 {
		chunk = total
	}
	for start := 0; start < total; start += chunk {
		end := start + chunk
		if end > total {
			end = total
		}
		for i := start; i < end; i++ {
			if err := ctx.Err(); err != nil {
				return report, err // honour cancellation / a resumable LRO stop
			}
			_, created, err := repo.Upsert(ctx, entities[i])
			switch {
			case err != nil:
				report.Failed++
				report.Errors = append(report.Errors, RowError{Index: i, Key: keyOf(entities[i]), Err: err})
				if opts.StopOnError {
					if opts.Progress != nil {
						opts.Progress(i+1, total)
					}
					return report, RowError{Index: i, Key: keyOf(entities[i]), Err: err}
				}
			case created:
				report.Created++
			default:
				report.Updated++
			}
		}
		if opts.Progress != nil {
			opts.Progress(end, total)
		}
	}
	return report, nil
}

// TopoSort orders items so every item appears after all items it references —
// parents before children — which is what natural-key reference resolution needs:
// a child row whose reference points at a parent's natural key must import after
// that parent exists. dependsOn returns the natural keys an item references;
// references to keys not present in items (already-imported, or external) are
// ignored. Order among independent items is preserved (stable). Returns ErrCycle
// if no such ordering exists.
func TopoSort[T any](items []T, keyOf func(T) NaturalKey, dependsOn func(T) []NaturalKey) ([]T, error) {
	n := len(items)
	idxByKey := make(map[NaturalKey]int, n)
	for i, it := range items {
		idxByKey[keyOf(it)] = i
	}
	// Build edges parent -> child and in-degrees, restricted to keys present here.
	indeg := make([]int, n)
	children := make([][]int, n)
	for child, it := range items {
		for _, dep := range dependsOn(it) {
			parent, ok := idxByKey[dep]
			if !ok || parent == child {
				continue // external/self reference: not an ordering constraint here
			}
			children[parent] = append(children[parent], child)
			indeg[child]++
		}
	}
	// Kahn's algorithm; iterate in input order so independent items stay stable.
	queue := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	out := make([]T, 0, n)
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		out = append(out, items[i])
		for _, c := range children[i] {
			indeg[c]--
			if indeg[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	if len(out) != n {
		return nil, ErrCycle
	}
	return out, nil
}
