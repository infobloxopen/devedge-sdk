// fence.go — L3 storage fencing for cell-based development on the GORM backend.
//
// Two pieces compose here:
//
//   - GormFencer is the controller-facing cells.Fencer: it records the
//     authoritative (owner cell, route epoch, seal) per tenant in the framework
//     tenant_fence table. Seal blocks all tenant-scoped writes for the move barrier;
//     SetOwner installs the only writer allowed and lifts the seal. Both are
//     forward-only on the epoch (cells.ErrFenceRegression on a backward epoch).
//
//   - The write-guard callback enforces that fence on every tenant-scoped mutation
//     in the SAME transaction as the write. It reads the writer's cells.AdmissionToken
//     from ctx; a writer with no token is ALLOWED (not cell-routed / not yet adopted),
//     so a service that has not turned on cell routing is unaffected. A writer WITH a
//     token is checked against the tenant_fence row: a sealed tenant is rejected, and a
//     token whose (cell, route epoch) does not match the row's owner is rejected — so a
//     stale or zombie writer from the old cell is stopped at the row even if it slipped
//     past the L1 router and the L2 gate.
//
// The guard composes with the existing GormTxRunner: it runs as a BEFORE callback on
// Create/Update/Delete, reading the fence row through db.Statement.ConnPool (the same
// transaction the write is issued on), so the fence read and the write share one commit.
package gormtx

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/infobloxopen/devedge-sdk/cells"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// ErrTenantFenced is the typed error the write-guard returns when a tenant-scoped
// write is rejected by the fence: the tenant is sealed for a move, or the writer's
// admission token (cell + route epoch) does not match the tenant's current owner.
// It aborts the write (set on the *gorm.DB via AddError), so the surrounding
// Atomically rolls back.
var ErrTenantFenced = errors.New("gormtx: tenant write rejected by storage fence")

// TenantFenceRow is the framework tenant_fence table: the authoritative storage
// fence per tenant. OwnerCell + RouteEpoch name the only writer allowed; Sealed
// blocks ALL writers during a move barrier; BarrierEpoch is the epoch the seal was
// installed at (forward-only). It is namespace-aware like the other framework tables
// (the write-guard and the fencer resolve a possibly-qualified table name).
type TenantFenceRow struct {
	TenantID     string `gorm:"primaryKey;column:tenant_id;type:varchar(255)"`
	OwnerCell    string `gorm:"column:owner_cell"`
	RouteEpoch   int64  `gorm:"column:route_epoch;default:0"`
	Sealed       bool   `gorm:"column:sealed;default:false"`
	BarrierEpoch int64  `gorm:"column:barrier_epoch;default:0"`
	UpdatedAt    time.Time
}

// TableName pins the fence table name.
func (TenantFenceRow) TableName() string { return "tenant_fence" }

// fenceBaseTable is the unqualified fence table name.
const fenceBaseTable = "tenant_fence"

// GormFencer is the GORM-backed cells.Fencer: it persists the per-tenant storage
// fence in the tenant_fence table. The move controller calls Seal/SetOwner; the
// write-guard reads the same table to admit or reject a tenant-scoped write.
//
// WS-012 P2 parity: the table name is RESOLVED from a persistence.DatabaseNamespace
// (WithFencerNamespace) so two co-resident modules sharing one database get isolated
// fences; the zero namespace yields the bare "tenant_fence".
type GormFencer struct {
	db    *gorm.DB
	now   func() time.Time
	table string
}

// FencerOption configures a GormFencer.
type FencerOption func(*GormFencer)

// WithFencerNamespace qualifies the fence table per a module's
// persistence.DatabaseNamespace. The zero namespace leaves the bare name.
func WithFencerNamespace(ns persistence.DatabaseNamespace) FencerOption {
	return func(f *GormFencer) { f.table = ns.QualifyTable(fenceBaseTable) }
}

// NewGormFencer returns a GORM-backed Fencer over db. Construct it with the same
// *gorm.DB the repositories and tx runner use so it reads/writes the same fence the
// write-guard enforces.
func NewGormFencer(db *gorm.DB, opts ...FencerOption) *GormFencer {
	f := &GormFencer{db: db, now: time.Now, table: fenceBaseTable}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Seal implements cells.Fencer: upsert {Sealed:true, BarrierEpoch} for tenantID,
// blocking all tenant-scoped writes for the move barrier. Forward-only:
// cells.ErrFenceRegression when barrierEpoch is below the stored barrier epoch.
// Idempotent on the same epoch.
func (f *GormFencer) Seal(ctx context.Context, tenantID string, barrierEpoch uint64) error {
	return f.upsert(ctx, tenantID, func(cur *TenantFenceRow) error {
		if barrierEpoch < uint64(cur.BarrierEpoch) {
			return cells.ErrFenceRegression
		}
		cur.Sealed = true
		cur.BarrierEpoch = int64(barrierEpoch)
		return nil
	})
}

// SetOwner implements cells.Fencer: upsert {OwnerCell, RouteEpoch, Sealed:false} for
// tenantID, installing the only writer allowed and lifting any seal (commit or
// rollback). Forward-only: cells.ErrFenceRegression when routeEpoch is below the
// stored route epoch. Idempotent on the same epoch.
func (f *GormFencer) SetOwner(ctx context.Context, tenantID, ownerCell string, routeEpoch uint64) error {
	return f.upsert(ctx, tenantID, func(cur *TenantFenceRow) error {
		if routeEpoch < uint64(cur.RouteEpoch) {
			return cells.ErrFenceRegression
		}
		cur.OwnerCell = ownerCell
		cur.RouteEpoch = int64(routeEpoch)
		cur.Sealed = false
		return nil
	})
}

// upsert reads the tenant's fence row (zero if absent), applies mutate, and writes
// it back, inside one transaction so the forward-only check and the write are atomic.
func (f *GormFencer) upsert(ctx context.Context, tenantID string, mutate func(*TenantFenceRow) error) error {
	return f.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur TenantFenceRow
		err := tx.Table(f.table).Where("tenant_id = ?", tenantID).Take(&cur).Error
		switch {
		case err == nil:
		case errors.Is(err, gorm.ErrRecordNotFound):
			cur = TenantFenceRow{TenantID: tenantID}
		default:
			return fmt.Errorf("read fence for %q: %w", tenantID, err)
		}
		if merr := mutate(&cur); merr != nil {
			return merr
		}
		cur.TenantID = tenantID
		cur.UpdatedAt = f.now()
		if serr := tx.Table(f.table).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "tenant_id"}},
			UpdateAll: true,
		}).Create(&cur).Error; serr != nil {
			return fmt.Errorf("write fence for %q: %w", tenantID, serr)
		}
		return nil
	})
}

// compile-time check.
var _ cells.Fencer = (*GormFencer)(nil)

// --- write-guard callback ----------------------------------------------------

const (
	writeGuardCallbackName = "devedge:tenant_write_guard"
	tenantFieldDBName      = "account_id"
)

// InstallTenantWriteGuard registers the L3 storage-fencing write-guard on db's
// Create/Update/Delete callbacks. After install, every tenant-scoped mutation on db
// (or on a transaction-scoped *gorm.DB derived from it, e.g. inside
// GormTxRunner.Atomically) is checked against the tenant_fence table in the same
// transaction before it runs.
//
// It composes with the existing GormTxRunner: the runner stashes the tx-scoped
// *gorm.DB and runs the writes through it; the callbacks fire on that *gorm.DB, and
// the guard reads the fence through db.Statement.ConnPool (the same transaction), so
// the fence read and the write share one commit.
//
// Models with no account_id field are skipped (the guard only fences tenant-scoped
// tables). A writer with no cells.AdmissionToken on ctx is ALLOWED (fail-open for a
// never-fenced / not-yet-cell-routed writer); a writer WITH a token is admitted only
// when the tenant has no fence row, or the row is unsealed and its (owner cell, route
// epoch) match the token.
//
// Pass WithFencerNamespace's resolved table via WithGuardTable for a namespaced
// module; the zero option uses the bare "tenant_fence".
func InstallTenantWriteGuard(db *gorm.DB, opts ...GuardOption) error {
	g := &writeGuard{table: fenceBaseTable}
	for _, opt := range opts {
		opt(g)
	}
	cb := db.Callback()
	if err := cb.Create().Before("gorm:create").Register(writeGuardCallbackName, g.check); err != nil {
		return fmt.Errorf("register write-guard on create: %w", err)
	}
	if err := cb.Update().Before("gorm:update").Register(writeGuardCallbackName, g.check); err != nil {
		return fmt.Errorf("register write-guard on update: %w", err)
	}
	if err := cb.Delete().Before("gorm:delete").Register(writeGuardCallbackName, g.check); err != nil {
		return fmt.Errorf("register write-guard on delete: %w", err)
	}
	return nil
}

// GuardOption configures the write-guard.
type GuardOption func(*writeGuard)

// WithGuardTable sets the (possibly namespaced) tenant_fence table the guard reads,
// to match a namespaced GormFencer. Defaults to the bare "tenant_fence".
func WithGuardTable(table string) GuardOption {
	return func(g *writeGuard) {
		if table != "" {
			g.table = table
		}
	}
}

// WithGuardNamespace is the namespace-aware form of WithGuardTable.
func WithGuardNamespace(ns persistence.DatabaseNamespace) GuardOption {
	return func(g *writeGuard) { g.table = ns.QualifyTable(fenceBaseTable) }
}

type writeGuard struct {
	table string
}

// check is the registered callback. It is intentionally conservative: it only acts
// when (a) the statement has a parsed schema with an account_id field, (b) a tenant
// value can be resolved from the statement's destination, and (c) the writer carries
// an admission token. Anything else falls through (allowed) so the guard never blocks
// a write it cannot reason about — fail-open for never-fenced writers, fail-closed
// only for an admitted writer that mismatches a real fence row.
func (g *writeGuard) check(db *gorm.DB) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	// Skip our own framework tables (the fence / allocator / outbox are not tenant
	// aggregates and must never be guarded — that would recurse / self-block).
	if isFrameworkTable(db.Statement.Table) {
		return
	}
	field, ok := db.Statement.Schema.FieldsByDBName[tenantFieldDBName]
	if !ok {
		return // not a tenant-scoped model
	}
	ctx := db.Statement.Context
	tok, ok := cells.AdmissionTokenFromContext(ctx)
	if !ok {
		return // writer not cell-routed → allow (fail-open for never-fenced writers)
	}
	tenantID, ok := tenantValueFromStatement(db, field)
	if !ok || tenantID == "" {
		// Cannot resolve the tenant from this statement (e.g. a blind UPDATE by a
		// non-tenant predicate). Allow: the ent mixin / row-scoped paths close the
		// cross-tenant gap; this guard fences the common create/update-by-model path.
		return
	}
	allowed, err := g.allow(db, ctx, tenantID, tok)
	if err != nil {
		_ = db.AddError(fmt.Errorf("tenant write-guard: %w", err))
		return
	}
	if !allowed {
		_ = db.AddError(fmt.Errorf("%w: tenant %q cell %q epoch %d", ErrTenantFenced, tenantID, tok.CellID, tok.RouteEpoch))
	}
}

// allow runs the fence predicate against the tenant_fence row in the SAME
// transaction as the pending write (db.Statement.ConnPool is the tx connection):
// no row ⇒ allow; sealed ⇒ reject; owner cell or route epoch mismatch ⇒ reject.
func (g *writeGuard) allow(db *gorm.DB, ctx context.Context, tenantID string, tok cells.AdmissionToken) (bool, error) {
	var row TenantFenceRow
	// Use a session bound to the statement's ConnPool so the read runs on the SAME
	// transaction as the write (not a fresh connection that cannot see uncommitted
	// fence changes within the move's tx, and so the read+write commit together).
	tx := db.Session(&gorm.Session{NewDB: true, Context: ctx})
	tx.Statement.ConnPool = db.Statement.ConnPool
	err := tx.Table(g.table).Where("tenant_id = ?", tenantID).Take(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return true, nil // no fence row → allow (fail-open for never-fenced tenant)
	case err != nil:
		return false, fmt.Errorf("read fence for %q: %w", tenantID, err)
	}
	if row.Sealed {
		return false, nil
	}
	return row.OwnerCell == tok.CellID && row.RouteEpoch == int64(tok.RouteEpoch), nil
}

// tenantValueFromStatement extracts the account_id value from the statement's
// destination (the model/struct being created or updated), if present and non-zero.
// It reads the field via the schema field's ValueOf over the statement's
// ReflectValue, handling a single struct destination (the common create/update path).
// A slice/batch or a map destination, or a zero value, returns ("", false) so the
// guard falls through (allowed) rather than guess a tenant it cannot read.
func tenantValueFromStatement(db *gorm.DB, field *schema.Field) (string, bool) {
	rv := db.Statement.ReflectValue
	if !rv.IsValid() {
		return "", false
	}
	switch rv.Kind() {
	case reflect.Struct:
		v, zero := field.ValueOf(db.Statement.Context, rv)
		if zero {
			return "", false
		}
		s, ok := v.(string)
		return s, ok
	default:
		// Slice (batch create) / Ptr / Map / Interface: not the single-row path this
		// guard fences. Allow; the row-scoped paths (ent mixin) close those gaps.
		return "", false
	}
}

// isFrameworkTable reports whether table is one of the SDK framework tables that
// must never be tenant-write-guarded. It matches the bare base names and any
// namespaced form (prefix or schema-qualified) by suffix.
func isFrameworkTable(table string) bool {
	for _, base := range []string{fenceBaseTable, eventSeqBaseTable, eventPolicyBaseTable, outboxBaseTable, cursorBaseTable, deadLetterBaseTable, idempotencyBaseTable} {
		if table == base || strings.HasSuffix(table, base) {
			return true
		}
	}
	return false
}
