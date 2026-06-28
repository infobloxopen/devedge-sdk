package iamv1_test

// ws012_namespace_pg_test.go — the WS-012 P2 ACCEPTANCE PROOF on REAL Postgres:
// two composable MODULES booted in ONE host on ONE database, proving their
// framework tables (outbox, idempotency_markers, the dispatcher cursor + dead-letter
// sidecars, each module's own schema_migrations) AND a same-named domain table do
// NOT collide — schema isolation (schema-preferred, the default). It drives the full
// servicekit.Run host path with the gormtx-backed MigrationRunner (host-run,
// advisory-locked, per-module-schema migration) and asserts the on-disk schema layout
// from the Postgres catalog.
//
// Docker-optional: it reuses startPostgres/freshPGDatabase (pgtest_test.go), which
// t.Skip() cleanly when Docker is unavailable — so `go test ./...` is green without
// Docker, and runs for real when Docker is up.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	"github.com/infobloxopen/devedge-sdk/servicekit"
)

// nsDomainModel is a trivial domain model BOTH modules define a same-named table
// for ("ns_domain_models" via the naming strategy). Under schema isolation each lands
// in its module's schema, so they never collide.
type nsDomainModel struct {
	ID    string `gorm:"primaryKey;type:varchar(36)"`
	Owner string
}

// nsTestModule is a minimal servicekit.Module: its Register reads the module's
// allocated DatabaseNamespace from app.DB and constructs namespaced framework stores
// over a per-module gorm handle (search_path scoped to the module schema), then
// records a health check. It mirrors what a generated gorm Module's Register does for
// the DB axis, without pulling the full IAM CRUD surface into this isolation proof.
type nsTestModule struct {
	id  string
	db  *gorm.DB // per-module handle, search_path = module schema
	out *gormtx.GormOutboxStore
}

func (m *nsTestModule) Descriptor() servicekit.Descriptor {
	return servicekit.Descriptor{
		ID:      m.id,
		Methods: []string{fmt.Sprintf("/%s.v1.Svc/Noop", m.id)},
		// One self-consistent public method so the server union gate passes (no repo
		// needed — this test is about the DB axis, not the CRUD surface).
		// (Public exemption avoids needing an authz rule.)
	}
}

func (m *nsTestModule) Register(_ context.Context, app *servicekit.App) error {
	method := fmt.Sprintf("/%s.v1.Svc/Noop", m.id)
	app.Server.RecordMethods(method)
	// Mark the method public so the server's union completeness gate is satisfied
	// without needing a real authz rule (this test exercises the DB axis).
	app.Server.AddRules(authz.MethodRule{Method: method, Public: true})
	return nil
}

// openSchemaScopedPG opens a gorm handle whose search_path is the module schema, so
// every table (framework + domain) resolves into that schema. This is the pool-safe
// schema-isolation wiring NamespacedPostgresDSN produces.
func openSchemaScopedPG(t *testing.T, dsn string, ns persistence.DatabaseNamespace) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(persistence.NamespacedPostgresDSN(dsn, ns)), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("gorm.Open postgres (schema %s): %v", ns.Schema, err)
	}
	t.Cleanup(func() {
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestWS012_TwoModules_SchemaIsolation_RealPostgres(t *testing.T) {
	baseDSN := freshPGDatabase(t, startPostgres(t)) // skips cleanly without Docker
	engine := "postgres"

	// Resolve a schema namespace per module (schema-preferred default on Postgres).
	ordersNS, err := persistence.ResolveNamespace(persistence.IsolationSchemaPreferred, "orders", engine, "", "")
	if err != nil {
		t.Fatal(err)
	}
	billingNS, err := persistence.ResolveNamespace(persistence.IsolationSchemaPreferred, "billing", engine, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ordersNS.Schema == "" || billingNS.Schema == "" || ordersNS.Schema == billingNS.Schema {
		t.Fatalf("expected two distinct schemas, got %q and %q", ordersNS.Schema, billingNS.Schema)
	}

	// Per-module schema-scoped handles (search_path = module schema).
	ordersDB := openSchemaScopedPG(t, baseDSN, ordersNS)
	billingDB := openSchemaScopedPG(t, baseDSN, billingNS)
	dbByModule := map[string]*gorm.DB{"orders": ordersDB, "billing": billingDB}

	fw := gormtx.MigrationModelsFor(true /*outbox*/, true /*idempotency*/)

	// The host's MigrationRunner: host-run, advisory-locked, per-module-schema. This
	// is exactly the seam servicekit.Run calls before each module registers.
	migrate := func(ctx context.Context, ns servicekit.DatabaseNamespace, _ servicekit.DatabaseDescriptor) error {
		return gormtx.MigrateModule(ctx, dbByModule[ns.ModuleID], gormtx.MigrateOptions{
			Namespace:       ns,
			DomainModels:    []any{&nsDomainModel{}},
			FrameworkModels: fw,
		})
	}

	// Build the two modules and run them through the FULL host path.
	ordersMod := &nsTestModule{id: "orders", db: ordersDB, out: gormtx.NewGormOutboxStore(ordersDB, gormtx.WithOutboxNamespace(ordersNS))}
	billingMod := &nsTestModule{id: "billing", db: billingDB, out: gormtx.NewGormOutboxStore(billingDB, gormtx.WithOutboxNamespace(billingNS))}

	// Drive the FULL host path: migrations (host-run, advisory-locked, per-module
	// schema) run inside Run BEFORE the boot gate, so we must not cancel until the
	// server is actually serving. Bind a known loopback port and dial it to detect
	// serving, then cancel for a clean return.
	addr := freeLoopbackAddrWS012(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- servicekit.Run(servicekit.HostConfig{
			Modules:  []servicekit.Module{ordersMod, billingMod},
			GRPCAddr: addr,
			Context:  ctx,
			Database: &servicekit.DatabaseConfig{Engine: engine, DefaultIsolation: servicekit.IsolationSchemaPreferred},
			Migrate:  migrate,
		})
	}()
	waitForListener(t, addr) // migration + boot completed once the port accepts
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("servicekit.Run (two modules, one PG): %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("servicekit.Run did not return within 30s")
	}

	// --- ASSERT the on-disk layout from the Postgres catalog ---
	admin, err := sql.Open("postgres", baseDSN)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()

	frameworkAndDomain := []string{
		"outbox", "idempotency_markers", "outbox_dispatch_cursor",
		"outbox_dead_letter", "schema_migrations", "ns_domain_models",
	}
	for _, schema := range []string{ordersNS.Schema, billingNS.Schema} {
		for _, table := range frameworkAndDomain {
			if !pgTableExists(t, admin, schema, table) {
				t.Errorf("expected table %s.%s to exist", schema, table)
			}
		}
	}
	// The framework tables must NOT exist in public (no module leaked to a shared,
	// colliding table).
	for _, table := range []string{"outbox", "idempotency_markers", "ns_domain_models", "schema_migrations"} {
		if pgTableExists(t, admin, "public", table) {
			t.Errorf("table public.%s exists — a module leaked into the shared namespace (collision risk)", table)
		}
	}

	// --- ASSERT behavioral isolation: an orders outbox append is invisible to billing ---
	ordersTx := gormtx.NewGormTxRunner(ordersDB)
	if err := ordersTx.Atomically(context.Background(), func(ctx context.Context) error {
		return ordersMod.out.Append(ctx, &persistence.OutboxRecord{
			ID: "evt-iso", AccountID: "t1", EventType: "orders.created", CreatedTime: time.Now(),
		})
	}); err != nil {
		t.Fatalf("orders append: %v", err)
	}
	ordersRecs, err := ordersMod.out.ReadAfter(context.Background(), persistence.OutboxCursor{}, 10)
	if err != nil {
		t.Fatalf("orders read: %v", err)
	}
	if len(ordersRecs) != 1 {
		t.Fatalf("orders outbox should hold exactly 1 row, got %d", len(ordersRecs))
	}
	billingRecs, err := billingMod.out.ReadAfter(context.Background(), persistence.OutboxCursor{}, 10)
	if err != nil {
		t.Fatalf("billing read: %v", err)
	}
	if len(billingRecs) != 0 {
		t.Fatalf("billing outbox must be empty (schema-isolated), got %d rows", len(billingRecs))
	}

	// --- ASSERT each module recorded its OWN migration state, in its OWN table ---
	for _, ns := range []persistence.DatabaseNamespace{ordersNS, billingNS} {
		var n int
		row := admin.QueryRow(fmt.Sprintf("SELECT count(*) FROM %s.schema_migrations WHERE version = $1", ns.Schema), "baseline:"+ns.ModuleID)
		if err := row.Scan(&n); err != nil {
			t.Fatalf("read %s.schema_migrations: %v", ns.Schema, err)
		}
		if n != 1 {
			t.Errorf("module %q should have exactly its own baseline migration stamp, got %d", ns.ModuleID, n)
		}
	}
}

// freeLoopbackAddrWS012 binds :0, reads the assigned port, closes, and returns the
// addr for servicekit.Run to bind (so we can dial it to detect serving).
func freeLoopbackAddrWS012(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

// waitForListener dials addr until it accepts a TCP connection (the server is
// serving — i.e. migrations + boot gate passed), or fails after a deadline.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not start listening on %s within 30s (migration or boot failed)", addr)
}

// pgTableExists reports whether schema.table exists via information_schema.
func pgTableExists(t *testing.T, db *sql.DB, schema, table string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2`,
		schema, table,
	).Scan(&n)
	if err != nil {
		t.Fatalf("check %s.%s exists: %v", schema, table, err)
	}
	return n > 0
}
