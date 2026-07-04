package entrepo

import (
	"context"
	"errors"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/mixin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/middleware/etag"
)

// softDeleteContextKey is the unexported key for the show_deleted flag in context.
type softDeleteContextKey struct{}

// TenantFilterer is implemented by generated ent query types that support
// tenant-scoped filtering. protoc-gen-ent emits WhereAccountID on each tenant
// resource's query type (in ent/<resource>_filter.ent.go); the TenantMixin
// interceptor below calls it. If a query type does not implement this interface
// the filter is silently skipped — which is why the method is generated, not
// left to the consumer (GH #39).
type TenantFilterer interface {
	WhereAccountID(id string)
}

// SetTenantFilter applies the account_id filter to q if q implements TenantFilterer
// and the tenantID is non-empty.
func SetTenantFilter(q ent.Query, tenantID string) {
	if tenantID == "" {
		return
	}
	if f, ok := q.(TenantFilterer); ok {
		f.WhereAccountID(tenantID)
	}
}

// TenantMixin is an ent schema mixin that adds an immutable account_id field
// and a query interceptor that automatically scopes queries to the tenant
// from context (via middleware.TenantIDFromContext).
//
// Generated schemas embed this mixin when the proto message has an account_id field.
type TenantMixin struct {
	mixin.Schema
}

func (TenantMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("account_id").
			NotEmpty().
			Immutable().
			Comment("Tenant discriminator — all queries are automatically scoped to this value."),
	}
}

func (TenantMixin) Interceptors() []ent.Interceptor {
	return []ent.Interceptor{
		ent.InterceptFunc(func(next ent.Querier) ent.Querier {
			return ent.QuerierFunc(func(ctx context.Context, q ent.Query) (ent.Value, error) {
				// Fail closed: a tenant-scoped read with no established tenant is
				// rejected unless the caller explicitly opted into a system/admin
				// operation via middleware.WithSystemContext. The verified principal
				// is the tenant authority (TenantIDFromContext); the account-id header
				// is only a fallback and can never widen scope.
				tenantID := middleware.TenantIDFromContext(ctx)
				if !middleware.IsSystemContext(ctx) {
					if tenantID == "" {
						return nil, status.Error(codes.PermissionDenied, "entrepo: no tenant on a tenant-scoped query")
					}
					SetTenantFilter(q, tenantID)
				}
				return next.Query(ctx, q)
			})
		}),
	}
}

// WithShowDeleted returns a context that instructs soft-delete interceptors to
// include soft-deleted entities in query results (AIP-148 show_deleted).
func WithShowDeleted(ctx context.Context) context.Context {
	return context.WithValue(ctx, softDeleteContextKey{}, true)
}

// showDeletedFromContext returns true when the context was tagged by WithShowDeleted.
func showDeletedFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(softDeleteContextKey{}).(bool)
	return v
}

// SoftDeleteFilterer is implemented by generated ent query types that support
// soft-delete filtering. protoc-gen-ent emits WhereDeleteTimeIsNil on each query type.
type SoftDeleteFilterer interface {
	WhereDeleteTimeIsNil()
}

// SetSoftDeleteFilter applies the soft-delete predicate to q when it implements
// SoftDeleteFilterer. Silently skipped when q does not implement the interface.
func SetSoftDeleteFilter(q ent.Query) {
	if f, ok := q.(SoftDeleteFilterer); ok {
		f.WhereDeleteTimeIsNil()
	}
}

// SoftDeleteMixin is an ent schema mixin that adds a nullable delete_time field
// and a query interceptor that automatically excludes soft-deleted entities
// unless the context was tagged with WithShowDeleted (AIP-148).
//
// Generated schemas embed this mixin when the proto message has a delete_time
// OUTPUT_ONLY Timestamp field.
type SoftDeleteMixin struct {
	mixin.Schema
}

func (SoftDeleteMixin) Fields() []ent.Field {
	return []ent.Field{
		field.Time("delete_time").
			Optional().
			Nillable().
			Comment("AIP-148 soft-delete timestamp; nil for live entities."),
	}
}

func (SoftDeleteMixin) Interceptors() []ent.Interceptor {
	return []ent.Interceptor{
		ent.InterceptFunc(func(next ent.Querier) ent.Querier {
			return ent.QuerierFunc(func(ctx context.Context, q ent.Query) (ent.Value, error) {
				if !showDeletedFromContext(ctx) {
					SetSoftDeleteFilter(q)
				}
				return next.Query(ctx, q)
			})
		}),
	}
}

// etagSetter is implemented by every generated mutation type for a resource that
// embeds EtagMixin (the mixin's etag field generates a SetEtag method). The hook
// below uses it to stamp a fresh token without depending on a concrete type.
type etagSetter interface {
	SetEtag(string)
}

// EtagMixin is an ent schema mixin that adds an opaque AIP-154 `etag` field and a
// mutation hook that stamps a fresh token (etag.New) on every Create and Update —
// so a resource's ETag changes whenever the resource changes. This mirrors the
// GORM storage layer, which stamps etag.New() on Create/Update, giving the two
// backends the same optimistic-concurrency behavior out of the box.
//
// Generated schemas embed this mixin when the proto message has a string `etag`
// field. The write path (persist + re-stamp) is fully automatic — there is no
// consumer code to write for it. A consumer's fromEnt mapping surfaces e.Etag on
// the proto so a Get returns the stable token a client echoes as If-Match; the
// If-Match precondition comparison itself stays the documented handler pattern,
// identical on both backends.
type EtagMixin struct {
	mixin.Schema
}

func (EtagMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("etag").
			Optional().
			Comment("AIP-154 opaque concurrency token; re-stamped on every write."),
	}
}

func (EtagMixin) Hooks() []ent.Hook {
	return []ent.Hook{
		func(next ent.Mutator) ent.Mutator {
			return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
				// Stamp a fresh token on create and every update. ent query
				// interceptors do not run for mutations, so this lives in a hook
				// (which does) — the write-path analogue of the Tenant/SoftDelete
				// query interceptors above.
				if m.Op().Is(ent.OpCreate | ent.OpUpdate | ent.OpUpdateOne) {
					if s, ok := m.(etagSetter); ok {
						s.SetEtag(etag.New())
					}
				}
				return next.Mutate(ctx, m)
			})
		},
	}
}

// ErrCrossTenantWrite is returned by the TenantWriteGuardMixin hook when a mutation
// targets a row whose account_id differs from the tenant on the context — a
// cross-tenant write. It aborts the mutation (and its transaction).
var ErrCrossTenantWrite = errors.New("entrepo: cross-tenant write rejected")

// tenantGuardMutation is the narrow slice of a generated mutation the
// TenantWriteGuardMixin hook needs: the operation, and the OLD account_id of the
// affected row read from the database. Keeping it narrow (vs the full ent.Mutation)
// makes the guard rule unit-testable without a generated client.
type tenantGuardMutation interface {
	Op() ent.Op
	OldField(ctx context.Context, name string) (ent.Value, error)
}

// checkTenantWrite is the rule behind TenantWriteGuardMixin: on a row-scoped update
// or delete, read the affected row's OLD account_id and reject when it differs from
// ctxTenant (the tenant on the request context). It fails closed:
//   - a system/admin operation (middleware.WithSystemContext) is ALLOWED to cross
//     tenants (the sanctioned cross-tenant opt-out);
//   - an ABSENT tenant on a tenant-scoped write is REJECTED (ErrCrossTenantWrite) —
//     it is NOT treated as "not tenant-scoped" (that was the fail-open defect);
//   - otherwise ALLOW when the old value matches ctxTenant (or cannot be read for a
//     batch op — see the limitation below), and REJECT a confirmed cross-tenant
//     single-row write.
//
// Limitation: ent's OldField is only defined for the *One operations (UpdateOne /
// DeleteOne), which is exactly the row-scoped path this guard closes. For batch
// Update / Delete, OldField returns an error (no single old row); the guard then
// ALLOWS, relying on the TenantMixin query interceptor (which itself fails closed
// on an absent tenant) to scope the predicate. This closes the common cross-tenant
// single-row write gap without changing batch semantics.
func checkTenantWrite(ctx context.Context, m tenantGuardMutation, ctxTenant string) error {
	if middleware.IsSystemContext(ctx) {
		return nil // trusted system/admin/background path → allow (explicit opt-out)
	}
	if ctxTenant == "" {
		return ErrCrossTenantWrite // fail closed: no established tenant on a tenant-scoped write
	}
	if !m.Op().Is(ent.OpUpdate | ent.OpUpdateOne | ent.OpDelete | ent.OpDeleteOne) {
		return nil // creates stamp account_id from ctx; queries are not mutations
	}
	old, err := m.OldField(ctx, "account_id")
	if err != nil {
		// Batch op (OldField undefined) or the row no longer exists: allow. The
		// TenantMixin interceptor scopes the read path; we do not block here.
		return nil
	}
	oldTenant, ok := old.(string)
	if !ok || oldTenant == "" {
		return nil
	}
	if oldTenant != ctxTenant {
		return ErrCrossTenantWrite
	}
	return nil
}

// TenantWriteGuardMixin is an ent schema mixin whose mutation hook rejects an update
// or delete of a row that belongs to a DIFFERENT tenant than the one on the request
// context (via middleware.TenantIDFromContext) — closing the cross-tenant-write gap
// the read-path TenantMixin interceptor does not cover (interceptors do not run for
// mutations). It is the ent analogue of the GORM storage write-guard's tenant check.
//
// Generated schemas embed this mixin alongside TenantMixin on a tenant-scoped
// resource. It adds NO field (TenantMixin owns account_id); it is hook-only.
//
// Scope note (cell-based development): this mixin closes the CROSS-TENANT-write gap.
// FULL epoch-fencing on the ent path — reading the shared tenant_fence table to
// reject a stale-epoch / sealed write the way the GORM write-guard does — is OUT OF
// SCOPE here and deliberately NOT half-built. An ent data-owning consumer (a later
// phase) wires the SAME tenant_fence table through the shared *sql.DB (the ent client
// and the GORM fencer share one database), so the fence is enforced once at the SQL
// layer for both backends rather than reimplemented as a second ent hook.
type TenantWriteGuardMixin struct {
	mixin.Schema
}

func (TenantWriteGuardMixin) Hooks() []ent.Hook {
	return []ent.Hook{
		func(next ent.Mutator) ent.Mutator {
			return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
				if gm, ok := m.(tenantGuardMutation); ok {
					if err := checkTenantWrite(ctx, gm, middleware.TenantIDFromContext(ctx)); err != nil {
						return nil, err
					}
				}
				return next.Mutate(ctx, m)
			})
		},
	}
}

// softDeleteKeyMutation is the narrow slice of a generated soft-delete mutation
// that SoftDeleteUniqueMixin's hook needs: read the delete_time transition and
// write the soft_delete_key marker. Keeping it narrow (vs the full ent.Mutation)
// makes the maintenance logic unit-testable without a generated client.
type softDeleteKeyMutation interface {
	Op() ent.Op
	SetSoftDeleteKey(string)
	DeleteTime() (time.Time, bool) // (value, set-in-this-mutation)
	DeleteTimeCleared() bool       // ClearDeleteTime() was called (undelete)
}

// applySoftDeleteKey is the maintenance rule behind SoftDeleteUniqueMixin: on
// soft-delete (delete_time set) it stamps a unique tombstone marker so the row
// leaves the live (account_id, <field>, "") unique namespace; on undelete
// (delete_time cleared) it resets the marker to "" so the row re-enters it.
// Other mutations leave the marker untouched.
func applySoftDeleteKey(m softDeleteKeyMutation) {
	if !m.Op().Is(ent.OpUpdate | ent.OpUpdateOne) {
		return // create defaults to "" (live); deletes/queries are irrelevant
	}
	if m.DeleteTimeCleared() {
		m.SetSoftDeleteKey("") // undelete → back into the live unique namespace
		return
	}
	if _, set := m.DeleteTime(); set {
		// Soft-delete → a fresh opaque, per-tombstone marker. Any two rows that
		// could collide on (account_id, <field>) were necessarily soft-deleted in
		// distinct operations (only one can be live at a time), so a fresh token
		// per soft-delete is collision-free. etag.New() is reused purely as an
		// opaque-token source.
		m.SetSoftDeleteKey(etag.New())
	}
}

// SoftDeleteUniqueMixin adds a `soft_delete_key` discriminator column and a
// mutation hook that maintains it, so a per-tenant `unique` field can be
// re-created after the holding row is soft-deleted. It is the MySQL-backend
// analogue of the partial unique index (`WHERE delete_time IS NULL`) used on
// PostgreSQL/SQLite: MySQL has no partial indexes, so uniqueness is enforced by
// the composite `(account_id, <field>, soft_delete_key)` instead — live rows all
// share `soft_delete_key=""` (uniqueness holds among live rows), soft-deleted
// rows each carry a distinct marker (so they never block re-creation).
//
// protoc-gen-ent embeds this mixin (alongside SoftDeleteMixin) only when
// generating for the MySQL dialect on a resource that is BOTH soft-delete AND
// per-tenant `unique`. On PostgreSQL/SQLite the partial index is used and this
// mixin is not emitted.
type SoftDeleteUniqueMixin struct {
	mixin.Schema
}

func (SoftDeleteUniqueMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("soft_delete_key").
			Default("").
			Comment("Soft-delete discriminator for per-tenant uniqueness on MySQL: \"\" while live, a unique marker once soft-deleted."),
	}
}

func (SoftDeleteUniqueMixin) Hooks() []ent.Hook {
	return []ent.Hook{
		func(next ent.Mutator) ent.Mutator {
			return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
				if sm, ok := m.(softDeleteKeyMutation); ok {
					applySoftDeleteKey(sm)
				}
				return next.Mutate(ctx, m)
			})
		},
	}
}
