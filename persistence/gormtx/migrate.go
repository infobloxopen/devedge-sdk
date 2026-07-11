// migrate.go — WS-012 P2 HOST-RUN, advisory-locked, per-module-namespaced
// migration for the GORM backend. A composable module owns its migration FILES /
// model set; the HOST runs them — never the module from init(). This file is the
// host's runner: it serializes concurrent hosts with a Postgres advisory lock keyed
// by the module ID, creates/validates the module's schema (or relies on its table
// prefix), AutoMigrates the framework + domain tables INTO the module namespace, and
// stamps the module's OWN migration-state table.
//
// Why AutoMigrate and not a versioned engine: the SDK's GORM path creates tables via
// AutoMigrate today (there is no golang-migrate/.sql pipeline wired in core). P2
// makes that AutoMigrate NAMESPACE-AWARE and HOST-OWNED; a module that ships
// versioned .sql files plugs them in through the embedded MigrationsFS seam the
// servicekit migrator drives (the host reads the FS), but the framework tables are
// always materialized here so a co-resident module never collides on them.
package gormtx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// ModuleMigration is the row of a module's OWN migration-state table (one per
// applied migration step). Each module records its state in its own table
// (DatabaseNamespace.MigrationTable) so two co-resident modules never share or
// clobber a single schema_migrations table — the no-cross-module-ownership rule.
type ModuleMigration struct {
	Version   string    `gorm:"primaryKey;column:version;type:varchar(255)"`
	AppliedAt time.Time `gorm:"column:applied_at"`
}

// MigrationModelsFor returns the FRAMEWORK models the host always AutoMigrates for a
// module that uses the outbox/idempotency machinery: the write-only outbox, the
// dispatcher's cursor + dead-letter sidecars, and the idempotency markers. They are
// returned as plain models; the namespacing is applied by the caller's
// search_path / table prefix (see MigrateModule), so the SAME models land in
// orders.* for one module and billing.* for another.
func MigrationModelsFor(useOutbox, useIdempotency bool) []any {
	var models []any
	if useOutbox {
		models = append(models, &OutboxRow{}, &OutboxCursorRow{}, &OutboxDeadLetterRow{})
	}
	if useIdempotency {
		models = append(models, &IdemMarker{})
	}
	return models
}

// RequestIdempotencyMigrationModels returns the FRAMEWORK model for the durable,
// exactly-once request-idempotency store (WS-043 / F048): the idempotency_keys
// table. It is a SEPARATE slice from MigrationModelsFor (like CellMigrationModels)
// so a service that uses only the event-dedup marker table keeps its exact
// framework-table set; a service that enables durable request idempotency appends
// this. The baseline (schemagen) composes it so the canonical DDL always includes
// the table.
func RequestIdempotencyMigrationModels() []any {
	return []any{&IdempotencyKeyRow{}}
}

// CellMigrationModels returns the FRAMEWORK models for cell-based development: the
// storage fence (tenant_fence), the per-tenant event-seq allocator (tenant_event_seq),
// and the publisher-policy table (tenant_event_policy). A service that adopts
// cell-based development appends these to its module's FrameworkModels (alongside
// MigrationModelsFor); a service that has not is unaffected (the columns added to the
// outbox default safely on existing rows, and these tables simply do not exist).
//
// They are returned separately from MigrationModelsFor so existing single-module
// migrations keep their exact framework-table set; the host composes both slices.
func CellMigrationModels() []any {
	return []any{&TenantFenceRow{}, &TenantEventSeqRow{}, &TenantEventPolicyRow{}}
}

// MigrateOptions configures a host-run module migration.
type MigrateOptions struct {
	// Namespace is the module's resolved isolation identity. When Schema is set the
	// migrator creates the schema and runs everything inside it; when TablePrefix is
	// set the framework stores prefix their tables and DomainModels are prefixed via
	// the gorm naming strategy of db (the caller supplies a prefixed-naming db).
	Namespace persistence.DatabaseNamespace
	// DomainModels are the module's own GORM models (e.g. WidgetModel) to AutoMigrate
	// alongside the framework tables. May be empty (a module with no GORM domain
	// tables, e.g. ent-backed or migration-file-driven).
	DomainModels []any
	// FrameworkModels are the SDK framework tables to materialize for the module
	// (outbox/idempotency). Use MigrationModelsFor; empty means none.
	FrameworkModels []any
	// SkipAdvisoryLock disables the Postgres advisory lock (for engines without one,
	// e.g. SQLite — the dev/test single-process path needs no cross-host fence).
	// On Postgres it should stay false.
	SkipAdvisoryLock bool
}

// MigrateModule runs one module's migration under the host's discipline (WS-012 §5.4):
//
//  1. acquire a Postgres advisory lock keyed by the module ID (so two hosts booting
//     the same composition do not migrate the same module concurrently);
//  2. ensure the module's schema exists (Postgres schema isolation);
//  3. AutoMigrate the framework + domain models INTO the module namespace;
//  4. record state in the module's OWN migration table (DatabaseNamespace.MigrationTable).
//
// db MUST already be scoped to the module's namespace for schema isolation (its
// search_path set to ns.Schema — see NamespacedPostgres / the servicekit registry),
// so AutoMigrate creates the framework tables (which pin TableName and bypass the
// naming strategy) in the right schema. For prefix isolation db carries the module's
// table-prefix naming strategy so domain models are prefixed; the framework models
// are prefixed by their stores at runtime and here by a per-call table override.
//
// It is idempotent: re-running it is a no-op (AutoMigrate is additive, the schema
// CREATE is IF NOT EXISTS, the migration stamp is upserted).
func MigrateModule(ctx context.Context, db *gorm.DB, opts MigrateOptions) error {
	ns := opts.Namespace
	if err := validateMigrate(ns); err != nil {
		return err
	}
	dialect := db.Dialector.Name()

	// (1) advisory lock — Postgres only; serialize concurrent hosts per module.
	if dialect == "postgres" && !opts.SkipAdvisoryLock {
		unlock, err := acquireModuleAdvisoryLock(ctx, db, ns.ModuleID)
		if err != nil {
			return err
		}
		defer unlock()
	}

	// (2) ensure the module schema exists (Postgres schema isolation).
	if ns.Schema != "" && dialect == "postgres" {
		if err := db.WithContext(ctx).Exec(fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q", ns.Schema)).Error; err != nil {
			return fmt.Errorf("gormtx: create schema %q for module %q: %w", ns.Schema, ns.ModuleID, err)
		}
	}

	// (3) AutoMigrate framework + domain models into the namespace.
	//
	// Framework models pin TableName (bypassing the naming strategy), so under
	// PREFIX isolation we must AutoMigrate them through an explicit prefixed table
	// name; under SCHEMA isolation the search_path on db already routes them. Domain
	// models honor the naming strategy, so the prefixed-naming db (or schema
	// search_path) places them correctly.
	if err := migrateDomain(ctx, db, opts.DomainModels); err != nil {
		return err
	}
	if err := migrateFramework(ctx, db, ns, opts.FrameworkModels); err != nil {
		return err
	}

	// (4) stamp the module's own migration table so the module records its state in
	// ITS table, never a shared schema_migrations.
	if err := stampModuleMigration(ctx, db, ns); err != nil {
		return err
	}
	return nil
}

// migrateDomain AutoMigrates the module's domain models. They honor db's naming
// strategy (table prefix) and search_path (schema), so no per-model table override
// is needed.
func migrateDomain(ctx context.Context, db *gorm.DB, models []any) error {
	if len(models) == 0 {
		return nil
	}
	if err := db.WithContext(ctx).AutoMigrate(models...); err != nil {
		return fmt.Errorf("gormtx: automigrate domain models: %w", err)
	}
	return nil
}

// migrateFramework AutoMigrates the framework models into the module namespace.
// Under schema isolation it relies on db's search_path; under prefix isolation it
// AutoMigrates each framework model through an explicit prefixed table name (because
// the framework models pin TableName and would otherwise ignore the prefix).
func migrateFramework(ctx context.Context, db *gorm.DB, ns persistence.DatabaseNamespace, models []any) error {
	if len(models) == 0 {
		return nil
	}
	if ns.TablePrefix != "" {
		// Prefix isolation: framework models pin TableName (bypassing the naming
		// strategy), so override the table name per model. AutoMigrate's
		// has-table detection keys off the model's pinned TableName, not the .Table()
		// override, so it would try to RE-create the prefixed table on a second run;
		// guard with an explicit HasTable check on the qualified name to stay
		// idempotent. (Schema isolation below routes via search_path and needs no guard.)
		for _, m := range models {
			base, ok := frameworkBaseTable(m)
			if !ok {
				return fmt.Errorf("gormtx: framework model %T has no known base table", m)
			}
			qualified := ns.QualifyTable(base)
			// Idempotency: AutoMigrate's has-table detection keys off the model's pinned
			// TableName, not the .Table() override, so a second run would try to RE-create
			// the prefixed table. Guard with a dialect-portable existence probe on the
			// qualified name (the test SQLite dialector's migrator lacks a working
			// HasTable, so we query the catalog ourselves).
			exists, eerr := tableExists(ctx, db, qualified)
			if eerr != nil {
				return eerr
			}
			if exists {
				continue // already materialized — idempotent re-run
			}
			if err := db.WithContext(ctx).Table(qualified).AutoMigrate(m); err != nil {
				return fmt.Errorf("gormtx: automigrate framework table %q: %w", qualified, err)
			}
		}
		return nil
	}
	// Schema isolation (search_path on db) or no isolation: bare TableName is fine.
	if err := db.WithContext(ctx).AutoMigrate(models...); err != nil {
		return fmt.Errorf("gormtx: automigrate framework models: %w", err)
	}
	return nil
}

// tableExists reports whether a (possibly schema-qualified) table exists, in a
// dialect-portable way. It is used by the prefix-isolation framework migration to
// stay idempotent independent of the ORM migrator's has-table support. qualified may
// be "schema.table" (Postgres) or a bare/prefixed name.
func tableExists(ctx context.Context, db *gorm.DB, qualified string) (bool, error) {
	schema, table := "", qualified
	if i := indexByte(qualified, '.'); i >= 0 {
		schema, table = qualified[:i], qualified[i+1:]
	}
	var n int64
	q := db.WithContext(ctx)
	switch db.Dialector.Name() {
	case "sqlite":
		err := q.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?", table).Scan(&n).Error
		return n > 0, wrapExists(err, qualified)
	case "postgres":
		if schema == "" {
			schema = "public"
		}
		err := q.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?", schema, table).Scan(&n).Error
		return n > 0, wrapExists(err, qualified)
	case "mysql":
		err := q.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&n).Error
		return n > 0, wrapExists(err, qualified)
	default:
		// Unknown dialect: report not-exists so AutoMigrate runs (and surfaces any
		// real error). This keeps the helper from blocking an unanticipated engine.
		return false, nil
	}
}

func wrapExists(err error, qualified string) error {
	if err != nil {
		return fmt.Errorf("gormtx: check table %q exists: %w", qualified, err)
	}
	return nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// frameworkBaseTable maps a framework model to its unqualified base table name so
// prefix isolation can override it.
func frameworkBaseTable(m any) (string, bool) {
	switch m.(type) {
	case *OutboxRow:
		return outboxBaseTable, true
	case *OutboxCursorRow:
		return cursorBaseTable, true
	case *OutboxDeadLetterRow:
		return deadLetterBaseTable, true
	case *IdemMarker:
		return idempotencyBaseTable, true
	case *IdempotencyKeyRow:
		return idempotencyKeysBaseTable, true
	case *TenantFenceRow:
		return fenceBaseTable, true
	case *TenantEventSeqRow:
		return eventSeqBaseTable, true
	case *TenantEventPolicyRow:
		return eventPolicyBaseTable, true
	default:
		return "", false
	}
}

// stampModuleMigration ensures the module's own migration-state table exists and
// records a baseline row, so the module's migration state lives in ITS table.
func stampModuleMigration(ctx context.Context, db *gorm.DB, ns persistence.DatabaseNamespace) error {
	table := ns.MigrationTable
	if table == "" {
		table = ns.QualifyTable("schema_migrations")
	}
	exists, eerr := tableExists(ctx, db, table)
	if eerr != nil {
		return eerr
	}
	if !exists {
		if err := db.WithContext(ctx).Table(table).AutoMigrate(&ModuleMigration{}); err != nil {
			return fmt.Errorf("gormtx: create module migration table %q: %w", table, err)
		}
	}
	// Upsert a baseline stamp so the table is non-empty and the module's state is
	// observable. Versioned .sql migrations (driven by the servicekit migrator over
	// the embedded FS) stamp their own versions through the same table.
	row := ModuleMigration{Version: "baseline:" + ns.ModuleID, AppliedAt: time.Now().UTC()}
	if err := db.WithContext(ctx).Table(table).
		Where("version = ?", row.Version).
		FirstOrCreate(&row).Error; err != nil {
		return fmt.Errorf("gormtx: stamp module migration %q: %w", table, err)
	}
	return nil
}

// acquireModuleAdvisoryLock takes a Postgres TRANSACTION-less session advisory lock
// keyed by the module ID, returning an unlock func. It pins a dedicated connection
// (the session that holds the lock must release it), so two hosts booting the same
// composition serialize their per-module migrations and never race to CREATE the
// same schema/tables. The key is namespaced under a P2 migration domain so it cannot
// collide with the relay leader lock (DefaultRelayLockName).
func acquireModuleAdvisoryLock(ctx context.Context, db *gorm.DB, moduleID string) (func(), error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("gormtx: resolve *sql.DB for migration lock: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("gormtx: pin migration-lock session: %w", err)
	}
	key := moduleMigrationLockKey(moduleID)
	// Blocking lock: a second host WAITS for the first to finish migrating rather
	// than skipping (skipping could serve before tables exist). pg_advisory_lock
	// blocks until acquired.
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("gormtx: pg_advisory_lock for module %q: %w", moduleID, err)
	}
	unlock := func() {
		// Best-effort unlock; closing the pinned connection releases the session
		// lock unconditionally (Postgres frees session advisory locks on session end).
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		_ = conn.Close()
	}
	return unlock, nil
}

// moduleMigrationLockKey derives the int64 advisory-lock key for a module's
// migration from its ID. It delegates to persistence.ModuleMigrationLockKey — the
// SINGLE lock authority shared with the versioned-SQL engine (persistence/migrate),
// so the AutoMigrate dev path and the SQL engine never fight over two different keys.
func moduleMigrationLockKey(moduleID string) int64 {
	return persistence.ModuleMigrationLockKey(moduleID)
}

// NamespacedPostgresDSN re-exports persistence.NamespacedPostgresDSN for gormtx
// callers (the host opens its gorm.DB on the returned DSN so the framework + domain
// tables land in the module schema). The canonical rule lives in the root package
// because it is engine-level and entrepo uses it too.
func NamespacedPostgresDSN(dsn string, ns persistence.DatabaseNamespace) string {
	return persistence.NamespacedPostgresDSN(dsn, ns)
}

// errNoModuleID is returned when a migration is requested without a module ID.
var errNoModuleID = errors.New("gormtx: MigrateModule requires a module ID in the namespace")

// validateMigrate is a small precondition guard reused by callers/tests.
func validateMigrate(ns persistence.DatabaseNamespace) error {
	if ns.ModuleID == "" {
		return errNoModuleID
	}
	return nil
}
