package gormtx_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// WS-012 P2 — DB module-namespacing, PREFIX-fallback path (SQLite, no Docker).
//
// These tests prove the load-bearing P2 guarantee on the always-available SQLite
// backend, using PREFIX isolation (the fallback for engines without schemas): two
// co-resident modules sharing ONE database get isolated framework tables (outbox,
// idempotency_markers, the dispatcher cursor + dead-letter sidecars, and each
// module's own schema_migrations) AND isolated same-named domain tables — nothing
// collides. The Postgres schema-isolation twin of this is in testdata/iam
// (Docker-gated). Single-module behavior (the zero namespace) is covered by the
// existing outbox/idempotency tests.

// widgetDomainModel is a tiny domain model two modules both define a same-named
// table for. Like the SDK's generated domain models (and UNLIKE the framework
// rows), it does NOT pin TableName — so it gets its table name from the naming
// strategy, and each module's TablePrefix keeps the two "widget_domain_models"
// apart. This is exactly how a real generated WidgetModel behaves.
type widgetDomainModel struct {
	ID    string `gorm:"primaryKey"`
	Owner string
}

// openSharedSQLite opens ONE shared-cache in-memory SQLite db both modules use. A
// fresh naming strategy per handle lets each module carry its own table prefix while
// hitting the same physical database.
func openSharedSQLite(t *testing.T, dsn string, prefix string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{
		Logger:         logger.Discard,
		NamingStrategy: schema.NamingStrategy{TablePrefix: prefix},
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

// sqliteTables lists the table names currently in the shared SQLite db.
func sqliteTables(t *testing.T, db *gorm.DB) map[string]bool {
	t.Helper()
	var names []string
	if err := db.WithContext(context.Background()).
		Raw("SELECT name FROM sqlite_master WHERE type='table'").Scan(&names).Error; err != nil {
		t.Fatalf("list sqlite tables: %v", err)
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func TestPrefixIsolation_TwoModules_FrameworkAndDomainTablesDoNotCollide(t *testing.T) {
	const dsn = "ws012_prefix_isolation"

	ordersNS, err := persistence.ResolveNamespace(persistence.IsolationPrefixRequired, "orders", "sqlite", "", "")
	if err != nil {
		t.Fatal(err)
	}
	billingNS, err := persistence.ResolveNamespace(persistence.IsolationPrefixRequired, "billing", "sqlite", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ordersNS.TablePrefix == billingNS.TablePrefix {
		t.Fatalf("module prefixes must differ: %q vs %q", ordersNS.TablePrefix, billingNS.TablePrefix)
	}

	// Each module opens its OWN gorm handle on the SAME physical db, carrying its
	// table prefix in the naming strategy (so domain models are prefixed).
	ordersDB := openSharedSQLite(t, dsn, ordersNS.TablePrefix)
	billingDB := openSharedSQLite(t, dsn, billingNS.TablePrefix)

	frameworkModels := gormtx.MigrationModelsFor(true /*outbox*/, true /*idempotency*/)

	// Host-run migration per module (advisory lock skipped on SQLite).
	if err := gormtx.MigrateModule(context.Background(), ordersDB, gormtx.MigrateOptions{
		Namespace:        ordersNS,
		DomainModels:     []any{&widgetDomainModel{}},
		FrameworkModels:  frameworkModels,
		SkipAdvisoryLock: true,
	}); err != nil {
		t.Fatalf("migrate orders: %v", err)
	}
	if err := gormtx.MigrateModule(context.Background(), billingDB, gormtx.MigrateOptions{
		Namespace:        billingNS,
		DomainModels:     []any{&widgetDomainModel{}},
		FrameworkModels:  frameworkModels,
		SkipAdvisoryLock: true,
	}); err != nil {
		t.Fatalf("migrate billing: %v", err)
	}

	// Every framework + domain + migration table must exist UNDER EACH MODULE'S
	// PREFIX — proving no collision (two modules, one DB, distinct tables).
	// widget_domain_models is the naming-strategy table name for widgetDomainModel.
	tables := sqliteTables(t, ordersDB)
	for _, base := range []string{"outbox", "idempotency_markers", "outbox_dispatch_cursor", "outbox_dead_letter", "schema_migrations", "widget_domain_models"} {
		ord := ordersNS.QualifyTable(base)
		bil := billingNS.QualifyTable(base)
		if !tables[ord] {
			t.Errorf("orders table %q missing", ord)
		}
		if !tables[bil] {
			t.Errorf("billing table %q missing", bil)
		}
		if ord == bil {
			t.Errorf("namespaced names collided for base %q: %q", base, ord)
		}
	}
	// The BARE (unqualified) names must NOT exist — proving nothing fell back to a
	// shared, colliding table.
	for _, bare := range []string{"outbox", "idempotency_markers", "widget_domain_models", "schema_migrations"} {
		if tables[bare] {
			t.Errorf("unqualified table %q exists — a module leaked into the shared namespace", bare)
		}
	}
}

// TestPrefixIsolation_OutboxAppendsAreIsolated proves the BEHAVIOR (not just DDL):
// an outbox append in module orders lands ONLY in orders' outbox, invisible to
// billing's namespaced store, on the same shared DB.
func TestPrefixIsolation_OutboxAppendsAreIsolated(t *testing.T) {
	const dsn = "ws012_prefix_outbox_isolation"

	ordersNS, _ := persistence.ResolveNamespace(persistence.IsolationPrefixRequired, "orders", "sqlite", "", "")
	billingNS, _ := persistence.ResolveNamespace(persistence.IsolationPrefixRequired, "billing", "sqlite", "", "")

	ordersDB := openSharedSQLite(t, dsn, ordersNS.TablePrefix)
	billingDB := openSharedSQLite(t, dsn, billingNS.TablePrefix)

	fw := gormtx.MigrationModelsFor(true, true)
	if err := gormtx.MigrateModule(context.Background(), ordersDB, gormtx.MigrateOptions{Namespace: ordersNS, FrameworkModels: fw, SkipAdvisoryLock: true}); err != nil {
		t.Fatalf("migrate orders: %v", err)
	}
	if err := gormtx.MigrateModule(context.Background(), billingDB, gormtx.MigrateOptions{Namespace: billingNS, FrameworkModels: fw, SkipAdvisoryLock: true}); err != nil {
		t.Fatalf("migrate billing: %v", err)
	}

	ordersOutbox := gormtx.NewGormOutboxStore(ordersDB, gormtx.WithOutboxNamespace(ordersNS))
	billingOutbox := gormtx.NewGormOutboxStore(billingDB, gormtx.WithOutboxNamespace(billingNS))
	ordersTx := gormtx.NewGormTxRunner(ordersDB)

	// Append one event through orders' transactional outbox.
	if err := ordersTx.Atomically(context.Background(), func(ctx context.Context) error {
		return ordersOutbox.Append(ctx, &persistence.OutboxRecord{
			ID: "evt-1", AccountID: "t1", EventType: "orders.created", CreatedTime: time.Now(),
		})
	}); err != nil {
		t.Fatalf("orders append: %v", err)
	}

	// orders' store sees it; billing's namespaced store does NOT.
	ordersRecs, err := ordersOutbox.ReadAfter(context.Background(), persistence.OutboxCursor{}, 10)
	if err != nil {
		t.Fatalf("orders read: %v", err)
	}
	if len(ordersRecs) != 1 || ordersRecs[0].ID != "evt-1" {
		t.Fatalf("orders outbox should hold exactly evt-1, got %+v", ordersRecs)
	}
	billingRecs, err := billingOutbox.ReadAfter(context.Background(), persistence.OutboxCursor{}, 10)
	if err != nil {
		t.Fatalf("billing read: %v", err)
	}
	if len(billingRecs) != 0 {
		t.Fatalf("billing outbox must be empty (isolated), got %d rows", len(billingRecs))
	}
}
