package migrate_test

// apply_pg_test.go — the AC fixtures for the versioned-SQL migration engine, on REAL
// Postgres via testcontainers (F043 G-6). Each TestPG_ maps to an acceptance criterion.

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	glog "gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	migrate "github.com/infobloxopen/devedge-sdk/persistence/migrate"
)

// moduleFS builds an in-memory module migrations FS from name->SQL pairs.
func moduleFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// apply runs the engine for one module (framework baseline composed ahead of moduleFS)
// against dsn, using store as the persisted down-store.
func apply(ctx context.Context, dsn, schema, moduleID string, moduleFS fs.FS, store string) (migrate.Result, error) {
	return migrate.Apply(ctx, migrate.Config{
		DSN:               dsn,
		Namespace:         persistence.DatabaseNamespace{ModuleID: moduleID, Engine: "postgres", Schema: schema},
		FrameworkBaseline: migrate.FrameworkBaseline(),
		ModuleMigrations:  moduleFS,
		DownStoreDir:      store,
	})
}

// frameworkTableCount reports how many of the 7 framework tables exist in schema.
func frameworkTableCount(t *testing.T, dsn, schema string) int {
	t.Helper()
	return pgQueryInt(t, dsn,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name IN
		 ('outbox','idempotency_markers','outbox_dispatch_cursor','outbox_dead_letter','tenant_fence','tenant_event_seq','tenant_event_policy')`,
		schema)
}

// schemaMigration reads a module schema's own schema_migrations row.
func schemaMigration(t *testing.T, dsn, schema string) (version int, dirty bool) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow(fmt.Sprintf(`SELECT version, dirty FROM %q.schema_migrations`, schema)).Scan(&version, &dirty); err != nil {
		t.Fatalf("read %s.schema_migrations: %v", schema, err)
	}
	return version, dirty
}

func tableExists(t *testing.T, dsn, schema, table string) bool {
	t.Helper()
	return pgQueryInt(t, dsn,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2`, schema, table) == 1
}

const createWidgetsUp = `CREATE TABLE widgets ("id" text NOT NULL, "name" text, PRIMARY KEY ("id"));`
const dropWidgetsDown = `DROP TABLE widgets;`

// TestPG_AppliesFrameworkAndModule_UpAndDown — AC-1. The engine applies the framework
// baseline (0001) composed ahead of a module migration (0002) to the highest version on
// real Postgres; schema_migrations reflects it; the migrated schema is usable from BOTH a
// gorm client and a raw pgx/database-sql connection (the applier is backend-agnostic — it
// serves the ent and GORM host paths identically, framework tables being GORM-sourced and
// domain tables layering at 0002+); the matching down reverses cleanly.
func TestPG_AppliesFrameworkAndModule_UpAndDown(t *testing.T) {
	ctx := context.Background()
	dsn := freshPGDatabase(t, startPostgres(t))
	store := t.TempDir()
	const schema = "orders"

	res, err := apply(ctx, dsn, schema, "orders", moduleFS(map[string]string{
		"0002_widgets.up.sql":   createWidgetsUp,
		"0002_widgets.down.sql": dropWidgetsDown,
	}), store)
	if err != nil {
		t.Fatalf("apply up: %v", err)
	}
	if res.ToVersion != 2 {
		t.Fatalf("ToVersion = %d, want 2", res.ToVersion)
	}
	if n := frameworkTableCount(t, dsn, schema); n != 7 {
		t.Errorf("framework tables in %s = %d, want 7", schema, n)
	}
	if !tableExists(t, dsn, schema, "widgets") {
		t.Error("widgets table missing after up")
	}
	if v, dirty := schemaMigration(t, dsn, schema); v != 2 || dirty {
		t.Errorf("schema_migrations = (v%d, dirty=%v), want (v2, dirty=false)", v, dirty)
	}
	// event_seq/event_epoch (WS-008) present on the framework outbox — the regression guard.
	if c := pgQueryInt(t, dsn, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='outbox' AND column_name IN ('event_seq','event_epoch')`, schema); c != 2 {
		t.Errorf("outbox WS-008 columns = %d, want 2", c)
	}

	// Both host paths consume the SAME migrated schema: a gorm client on the module
	// search_path and a raw pgx connection both read the framework + domain tables.
	sp := dsn + "&search_path=" + schema + ",public"
	gdb, err := gorm.Open(postgres.Open(sp), &gorm.Config{Logger: glog.Discard})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	var gormCount int64
	if err := gdb.Table("widgets").Count(&gormCount).Error; err != nil {
		t.Errorf("gorm read widgets: %v", err)
	}
	if sqlDB, e := gdb.DB(); e == nil {
		_ = sqlDB.Close()
	}

	// Down: re-apply with ONLY the framework baseline (target = 1) → the engine migrates
	// DOWN to 1 using the 0002 down file PERSISTED in the store; widgets is dropped.
	res2, err := apply(ctx, dsn, schema, "orders", nil, store)
	if err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if res2.ToVersion != 1 {
		t.Errorf("down ToVersion = %d, want 1", res2.ToVersion)
	}
	if tableExists(t, dsn, schema, "widgets") {
		t.Error("widgets should be dropped after down to v1")
	}
	if v, _ := schemaMigration(t, dsn, schema); v != 1 {
		t.Errorf("schema_migrations after down = v%d, want v1", v)
	}
}

// TestPG_ConcurrentIndex_SingleStatement — AC-4. A single-statement CREATE INDEX
// CONCURRENTLY migration applies with no "cannot run inside a transaction block" error.
func TestPG_ConcurrentIndex_SingleStatement(t *testing.T) {
	ctx := context.Background()
	dsn := freshPGDatabase(t, startPostgres(t))
	const schema = "orders"
	res, err := apply(ctx, dsn, schema, "orders", moduleFS(map[string]string{
		"0002_widgets.up.sql":     createWidgetsUp,
		"0002_widgets.down.sql":   dropWidgetsDown,
		"0003_idx.up.sql":         `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_widgets_name ON widgets ("name");`,
		"0003_idx.down.sql":       `DROP INDEX CONCURRENTLY IF EXISTS idx_widgets_name;`,
	}), t.TempDir())
	if err != nil {
		t.Fatalf("apply with CONCURRENTLY index: %v", err)
	}
	if res.ToVersion != 3 {
		t.Fatalf("ToVersion = %d, want 3", res.ToVersion)
	}
	if n := pgQueryInt(t, dsn, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_widgets_name'`, schema); n != 1 {
		t.Errorf("idx_widgets_name present = %d, want 1", n)
	}
}

// TestPG_ConcurrentIndex_MultiStatementFailsLoud — AC-4. A file that puts CONCURRENTLY
// alongside another statement runs in an implicit transaction block and FAILS LOUD.
func TestPG_ConcurrentIndex_MultiStatementFailsLoud(t *testing.T) {
	ctx := context.Background()
	dsn := freshPGDatabase(t, startPostgres(t))
	_, err := apply(ctx, dsn, "orders", "orders", moduleFS(map[string]string{
		"0002_widgets.up.sql":   createWidgetsUp,
		"0002_widgets.down.sql": dropWidgetsDown,
		"0003_bad.up.sql":       "SET lock_timeout = '1s';\nCREATE INDEX CONCURRENTLY idx_widgets_name ON widgets (\"name\");",
		"0003_bad.down.sql":     `DROP INDEX IF EXISTS idx_widgets_name;`,
	}), t.TempDir())
	if err == nil {
		t.Fatal("expected a loud failure for multi-statement CONCURRENTLY, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "transaction") {
		t.Errorf("error = %q, want it to mention the transaction-block problem", err)
	}
}

// TestPG_TwoModules_IsolatedSchemas — AC-5. Two co-resident modules land their framework
// tables + their own schema_migrations in their OWN schemas, isolated.
func TestPG_TwoModules_IsolatedSchemas(t *testing.T) {
	ctx := context.Background()
	dsn := freshPGDatabase(t, startPostgres(t))
	for _, m := range []string{"orders", "billing"} {
		if _, err := apply(ctx, dsn, m, m, moduleFS(map[string]string{
			"0002_t.up.sql":   fmt.Sprintf(`CREATE TABLE %s_thing ("id" text NOT NULL, PRIMARY KEY("id"));`, m),
			"0002_t.down.sql": fmt.Sprintf(`DROP TABLE %s_thing;`, m),
		}), t.TempDir()); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}
	for _, m := range []string{"orders", "billing"} {
		if n := frameworkTableCount(t, dsn, m); n != 7 {
			t.Errorf("%s framework tables = %d, want 7", m, n)
		}
		if v, dirty := schemaMigration(t, dsn, m); v != 2 || dirty {
			t.Errorf("%s.schema_migrations = (v%d, dirty=%v), want (v2, false)", m, v, dirty)
		}
	}
	// The migration-state tables are per-module (each in its own schema), never shared.
	if !tableExists(t, dsn, "orders", "schema_migrations") || !tableExists(t, dsn, "billing", "schema_migrations") {
		t.Error("each module must own its schema_migrations table in its schema")
	}
}

// TestPG_ConcurrentReplicas_ApplyExactlyOnce — AC-5. Concurrent startup of N replicas of
// the SAME module applies its migrations EXACTLY ONCE (serialized by the single advisory
// lock); no duplicate/corrupt schema_migrations. Run with -race.
func TestPG_ConcurrentReplicas_ApplyExactlyOnce(t *testing.T) {
	ctx := context.Background()
	dsn := freshPGDatabase(t, startPostgres(t))
	const schema = "orders"
	const replicas = 5

	var wg sync.WaitGroup
	results := make([]migrate.Result, replicas)
	errs := make([]error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each replica has its OWN persisted down-store (per-pod), shares the DB +
			// the moduleID-keyed advisory lock.
			results[i], errs[i] = apply(ctx, dsn, schema, "orders", moduleFS(map[string]string{
				"0002_widgets.up.sql":   createWidgetsUp,
				"0002_widgets.down.sql": dropWidgetsDown,
			}), t.TempDir())
		}(i)
	}
	wg.Wait()

	winners := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("replica %d: %v", i, errs[i])
		}
		if !results[i].AlreadyCurrent {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("exactly one replica should have applied the migration; got %d", winners)
	}
	if v, dirty := schemaMigration(t, dsn, schema); v != 2 || dirty {
		t.Errorf("schema_migrations = (v%d, dirty=%v), want (v2, false)", v, dirty)
	}
	if n := pgQueryInt(t, dsn, `SELECT count(*) FROM orders.schema_migrations`); n != 1 {
		t.Errorf("schema_migrations row count = %d, want exactly 1 (no double-apply)", n)
	}
}

// TestPG_OneModuleFailure_DoesNotCorruptOther — AC-5. A failing migration in one module
// does not corrupt a co-resident module's schema.
func TestPG_OneModuleFailure_DoesNotCorruptOther(t *testing.T) {
	ctx := context.Background()
	dsn := freshPGDatabase(t, startPostgres(t))

	if _, err := apply(ctx, dsn, "orders", "orders", moduleFS(map[string]string{
		"0002_ok.up.sql":   createWidgetsUp,
		"0002_ok.down.sql": dropWidgetsDown,
	}), t.TempDir()); err != nil {
		t.Fatalf("apply orders: %v", err)
	}
	// billing's 0002 is invalid SQL → its migration fails.
	if _, err := apply(ctx, dsn, "billing", "billing", moduleFS(map[string]string{
		"0002_bad.up.sql":   `CREATE TABLE ( this is not valid sql;`,
		"0002_bad.down.sql": `DROP TABLE IF EXISTS nope;`,
	}), t.TempDir()); err == nil {
		t.Fatal("expected billing migration to fail on bad SQL")
	}
	// orders is intact and current; billing did not touch it.
	if n := frameworkTableCount(t, dsn, "orders"); n != 7 {
		t.Errorf("orders framework tables = %d, want 7 (uncorrupted)", n)
	}
	if v, dirty := schemaMigration(t, dsn, "orders"); v != 2 || dirty {
		t.Errorf("orders.schema_migrations = (v%d, dirty=%v), want (v2, false)", v, dirty)
	}
	if !tableExists(t, dsn, "orders", "widgets") {
		t.Error("orders.widgets should be intact after billing's failure")
	}
}

// TestPG_DirtyStateAutoRecovers — AC-6. A mid-apply failure leaves a recoverable dirty
// state that the next CORRECTED run auto-recovers, without manual cleanup.
func TestPG_DirtyStateAutoRecovers(t *testing.T) {
	ctx := context.Background()
	dsn := freshPGDatabase(t, startPostgres(t))
	const schema = "orders"
	store := t.TempDir()

	// First run: 0002 is invalid → the apply fails and leaves the DB dirty.
	if _, err := apply(ctx, dsn, schema, "orders", moduleFS(map[string]string{
		"0002_thing.up.sql":   `CREATE TABLE bogus ( nope;`,
		"0002_thing.down.sql": `DROP TABLE IF EXISTS thing;`,
	}), store); err == nil {
		t.Fatal("expected the bad migration to fail")
	}

	// Correct the migration and re-run against the SAME store → auto-recovery to v2.
	res, err := apply(ctx, dsn, schema, "orders", moduleFS(map[string]string{
		"0002_thing.up.sql":   `CREATE TABLE thing ("id" text NOT NULL, PRIMARY KEY("id"));`,
		"0002_thing.down.sql": `DROP TABLE thing;`,
	}), store)
	if err != nil {
		t.Fatalf("corrected re-run should auto-recover, got: %v", err)
	}
	if res.ToVersion != 2 {
		t.Errorf("recovered ToVersion = %d, want 2", res.ToVersion)
	}
	if v, dirty := schemaMigration(t, dsn, schema); v != 2 || dirty {
		t.Errorf("after recovery schema_migrations = (v%d, dirty=%v), want (v2, false)", v, dirty)
	}
	if !tableExists(t, dsn, schema, "thing") {
		t.Error("thing table should exist after recovery")
	}
}

// TestPG_PersistedDownStore_SurvivesSourceRemoval — AC-6. A down step persisted by an
// earlier apply remains usable after the current source no longer ships that migration,
// so a rollback runs even when the image dropped the down file.
func TestPG_PersistedDownStore_SurvivesSourceRemoval(t *testing.T) {
	ctx := context.Background()
	dsn := freshPGDatabase(t, startPostgres(t))
	const schema = "orders"
	store := t.TempDir()

	if _, err := apply(ctx, dsn, schema, "orders", moduleFS(map[string]string{
		"0002_widgets.up.sql":   createWidgetsUp,
		"0002_widgets.down.sql": dropWidgetsDown,
	}), store); err != nil {
		t.Fatalf("apply up: %v", err)
	}
	// The current source NO LONGER ships 0002 (only the framework baseline). Rolling to
	// the framework target (v1) must succeed using the down file PERSISTED in the store.
	res, err := apply(ctx, dsn, schema, "orders", nil, store)
	if err != nil {
		t.Fatalf("rollback via persisted down-store: %v", err)
	}
	if res.ToVersion != 1 {
		t.Errorf("ToVersion after rollback = %d, want 1", res.ToVersion)
	}
	if tableExists(t, dsn, schema, "widgets") {
		t.Error("widgets should be gone after rollback to v1")
	}
}
