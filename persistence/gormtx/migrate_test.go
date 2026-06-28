package gormtx_test

import (
	"context"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// openMigrateDB opens a shared-cache in-memory SQLite db with the module's table
// prefix in its naming strategy (so domain models are prefixed).
func openMigrateDB(t *testing.T, dsn, prefix string) *gorm.DB {
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

// TestMigrateModule_Idempotent proves a host can run a module's migration twice
// (e.g. two host boots) without error — the schema CREATE is IF NOT EXISTS,
// AutoMigrate is additive, and the migration stamp is upserted.
func TestMigrateModule_Idempotent(t *testing.T) {
	ns, err := persistence.ResolveNamespace(persistence.IsolationPrefixRequired, "orders", "sqlite", "", "")
	if err != nil {
		t.Fatal(err)
	}
	db := openMigrateDB(t, "ws012_migrate_idempotent", ns.TablePrefix)
	opts := gormtx.MigrateOptions{
		Namespace:        ns,
		FrameworkModels:  gormtx.MigrationModelsFor(true, true),
		SkipAdvisoryLock: true,
	}
	for i := 0; i < 2; i++ {
		if err := gormtx.MigrateModule(context.Background(), db, opts); err != nil {
			t.Fatalf("MigrateModule run %d: %v", i, err)
		}
	}
	// The module's own migration table exists and holds exactly one baseline stamp
	// (idempotent re-run did not duplicate it).
	var n int64
	if err := db.WithContext(context.Background()).
		Table(ns.MigrationTable).
		Where("version = ?", "baseline:orders").
		Count(&n).Error; err != nil {
		t.Fatalf("count migration stamps: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 baseline stamp after 2 runs (idempotent), got %d", n)
	}
}

// TestMigrateModule_RequiresModuleID guards the precondition.
func TestMigrateModule_RequiresModuleID(t *testing.T) {
	db := openMigrateDB(t, "ws012_migrate_noid", "")
	err := gormtx.MigrateModule(context.Background(), db, gormtx.MigrateOptions{
		Namespace:        persistence.DatabaseNamespace{}, // no ModuleID
		SkipAdvisoryLock: true,
	})
	if err == nil {
		t.Fatal("MigrateModule with no module ID should error")
	}
}

// TestMigrationModelsFor controls which framework tables a module materializes.
func TestMigrationModelsFor(t *testing.T) {
	if got := len(gormtx.MigrationModelsFor(false, false)); got != 0 {
		t.Errorf("no flags => 0 models, got %d", got)
	}
	if got := len(gormtx.MigrationModelsFor(true, false)); got != 3 { // outbox + cursor + dead-letter
		t.Errorf("outbox only => 3 models, got %d", got)
	}
	if got := len(gormtx.MigrationModelsFor(false, true)); got != 1 { // idempotency only
		t.Errorf("idempotency only => 1 model, got %d", got)
	}
	if got := len(gormtx.MigrationModelsFor(true, true)); got != 4 {
		t.Errorf("both => 4 models, got %d", got)
	}
}
