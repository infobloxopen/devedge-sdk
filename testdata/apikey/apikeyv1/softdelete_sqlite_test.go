package apikeyv1_test

// softdelete_sqlite_test.go — GORM integration tests for F020 soft-delete
// and PurgeExpired using an inline SQLite database.
// AC-008, AC-011.

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
)

// openSoftDeleteDB opens an in-memory SQLite GORM database with the APIKeyModel
// schema migrated. Each test should use a unique DSN to avoid cross-test state.
func openSoftDeleteDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open soft-delete test db: %v", err)
	}
	if err := db.AutoMigrate(&apikeyv1.APIKeyModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// TestSoftDelete_GORM_BasicCycle exercises the core AIP-148/149 lifecycle:
// Create → Delete (soft) → Get=NotFound → List default excludes → List show_deleted
// includes with non-nil DeleteTime → Undelete → Get succeeds with nil DeleteTime.
// AC-008.
func TestSoftDelete_GORM_BasicCycle(t *testing.T) {
	db := openSoftDeleteDB(t, "sd_basic")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := middleware.WithTenantID(context.Background(), "tenant1")

	// Create.
	k := &apikeyv1.APIKey{
		Id:        "sd-key-1",
		AccountId: "tenant1",
		Label:     "test key",
		KeyValue:  "sk_test",
	}
	created, err := repo.Create(ctx, k)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.DeleteTime != nil {
		t.Error("DeleteTime must be nil for a live entity")
	}

	// Soft delete.
	if err := repo.Delete(ctx, "sd-key-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Get returns NotFound (soft-deleted row excluded by default scope).
	if _, err := repo.Get(ctx, "sd-key-1"); err != persistence.ErrNotFound {
		t.Fatalf("Get after Delete: want ErrNotFound, got %v", err)
	}

	// Second Delete returns NotFound (already soft-deleted; default scope misses it).
	if err := repo.Delete(ctx, "sd-key-1"); err != persistence.ErrNotFound {
		t.Fatalf("second Delete: want ErrNotFound, got %v", err)
	}

	// List default excludes the soft-deleted row.
	items, _, err := repo.List(ctx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range items {
		if item.Id == "sd-key-1" {
			t.Error("List default must not include soft-deleted row")
		}
	}

	// List with ShowDeleted includes the row and sets DeleteTime.
	shown, _, err := repo.List(ctx, persistence.ListOptions{ShowDeleted: true})
	if err != nil {
		t.Fatalf("List ShowDeleted: %v", err)
	}
	var found *apikeyv1.APIKey
	for _, item := range shown {
		if item.Id == "sd-key-1" {
			found = item
		}
	}
	if found == nil {
		t.Fatal("List ShowDeleted: expected soft-deleted row to be present")
	}
	if found.DeleteTime == nil {
		t.Error("List ShowDeleted: DeleteTime must be non-nil for soft-deleted row")
	}

	// Undelete on a live row (using a different key) returns NotFound.
	live := &apikeyv1.APIKey{Id: "sd-key-2", AccountId: "tenant1", KeyValue: "sk_live"}
	if _, err := repo.Create(ctx, live); err != nil {
		t.Fatalf("Create live key: %v", err)
	}
	if _, err := repo.Undelete(ctx, "sd-key-2"); err != persistence.ErrNotFound {
		t.Fatalf("Undelete live: want ErrNotFound, got %v", err)
	}

	// Undelete restores the soft-deleted row.
	restored, err := repo.Undelete(ctx, "sd-key-1")
	if err != nil {
		t.Fatalf("Undelete: %v", err)
	}
	if restored.DeleteTime != nil {
		t.Error("Undelete: DeleteTime must be nil after restore")
	}

	// After Undelete, Get succeeds and List default includes it again.
	if _, err := repo.Get(ctx, "sd-key-1"); err != nil {
		t.Fatalf("Get after Undelete: %v", err)
	}
	after, _, _ := repo.List(ctx, persistence.ListOptions{})
	found = nil
	for _, item := range after {
		if item.Id == "sd-key-1" {
			found = item
		}
	}
	if found == nil {
		t.Error("List after Undelete: expected restored row to appear in default list")
	}
}

// TestSoftDelete_GORM_PurgeExpired verifies PurgeExpired hard-deletes rows past
// their expire_time, and that Undelete returns NotFound afterwards.
// AC-011.
func TestSoftDelete_GORM_PurgeExpired(t *testing.T) {
	db := openSoftDeleteDB(t, "sd_purge")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := middleware.WithTenantID(context.Background(), "tenant1")

	// Create a key, then manually set expire_time to the past by updating
	// the model directly (the proto API doesn't expose set-expire_time — it's
	// OUTPUT_ONLY, but a real service would set it on create/update at the
	// storage layer). We set it to 1 second ago via raw GORM.
	k := &apikeyv1.APIKey{Id: "purge-key-1", AccountId: "tenant1", Label: "expiring key"}
	if _, err := repo.Create(ctx, k); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pastTime := time.Now().Add(-1 * time.Second)
	if err := db.Model(&apikeyv1.APIKeyModel{}).
		Where("id = ?", "purge-key-1").
		Update("expire_time", pastTime).Error; err != nil {
		t.Fatalf("set expire_time: %v", err)
	}

	// Soft-delete so the row is eligible for purge.
	if err := repo.Delete(ctx, "purge-key-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// PurgeExpired should remove the row and return count=1.
	count, err := repo.PurgeExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if count != 1 {
		t.Fatalf("PurgeExpired: want count=1, got %d", count)
	}

	// Undelete on a purged row returns NotFound.
	if _, err := repo.Undelete(ctx, "purge-key-1"); err != persistence.ErrNotFound {
		t.Fatalf("Undelete after purge: want ErrNotFound, got %v", err)
	}
}
