package apikeyv1_test

// softdelete_unique_sqlite_test.go — #49 follow-up: verify, end-to-end on
// SQLite, that a per-tenant `unique` key on a soft-delete resource is
// re-creatable once the holder is soft-deleted, for BOTH GORM strategies:
//   - partial unique index (WHERE deleted_at IS NULL) — PostgreSQL/SQLite default;
//   - soft_delete_key discriminator column — the MySQL path (no partial indexes),
//     a dialect-agnostic SQL mechanism, so SQLite exercises it faithfully.
//
// The models below mirror exactly what protoc-gen-storage emits in each mode
// (the emitted gorm tags / column are asserted by the plugin's render tests);
// this file proves the database actually behaves as intended — in particular
// that GORM honors the partial `where` index tag.

import (
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// partialUniqueGORM mirrors the default (postgres/sqlite) protoc-gen-storage
// output: the composite unique index carries where:deleted_at IS NULL.
type partialUniqueGORM struct {
	ID        string         `gorm:"primaryKey;type:varchar(36)"`
	AccountID string         `gorm:"column:account_id;uniqueIndex:ux_pu_account_ref,priority:1,option:WHERE deleted_at IS NULL"`
	Ref       string         `gorm:"column:ref;uniqueIndex:ux_pu_account_ref,priority:2"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

func (partialUniqueGORM) TableName() string { return "partial_unique" }

// sentinelUniqueGORM mirrors the dialect=mysql output: a soft_delete_key column
// joins the composite as the trailing column (live rows share "").
type sentinelUniqueGORM struct {
	ID            string         `gorm:"primaryKey;type:varchar(36)"`
	AccountID     string         `gorm:"column:account_id;uniqueIndex:ux_sn_account_ref,priority:1"`
	Ref           string         `gorm:"column:ref;uniqueIndex:ux_sn_account_ref,priority:2"`
	SoftDeleteKey string         `gorm:"column:soft_delete_key;uniqueIndex:ux_sn_account_ref,priority:3"`
	DeletedAt     gorm.DeletedAt `gorm:"index"`
}

func (sentinelUniqueGORM) TableName() string { return "sentinel_unique" }

func openUniqueDB(t *testing.T, dsn string, models ...interface{}) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// A (partial index): GORM must create a PARTIAL unique index from the where tag,
// so the key frees on soft-delete. If GORM ignored the tag (full index) the
// re-create below would fail — this is the test that proves GORM-A actually works.
func TestGORMPartialUnique_RecreateAfterSoftDelete(t *testing.T) {
	db := openUniqueDB(t, "gorm_partial_unique", &partialUniqueGORM{})
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	if err := db.Create(&partialUniqueGORM{ID: "f1", AccountID: "t1", Ref: "r1"}).Error; err != nil {
		t.Fatalf("create f1: %v", err)
	}
	// Two LIVE rows with the same (account_id, ref) must still conflict.
	if err := db.Create(&partialUniqueGORM{ID: "f1b", AccountID: "t1", Ref: "r1"}).Error; err == nil {
		t.Fatal("two live rows sharing ref must violate the unique index")
	}
	if err := db.Delete(&partialUniqueGORM{}, "id = ?", "f1").Error; err != nil {
		t.Fatalf("soft-delete f1: %v", err)
	}
	if err := db.Create(&partialUniqueGORM{ID: "f2", AccountID: "t1", Ref: "r1"}).Error; err != nil {
		t.Fatalf("re-create ref=r1 after soft-delete must succeed (partial index): %v", err)
	}
}

// B (sentinel column): stamping the row id into soft_delete_key on soft-delete
// (what the generated Delete does) frees the key, while two live rows (both "")
// still conflict. Proves the MySQL mechanism on SQLite.
func TestGORMSentinelUnique_RecreateAfterSoftDelete(t *testing.T) {
	db := openUniqueDB(t, "gorm_sentinel_unique", &sentinelUniqueGORM{})
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	if err := db.Create(&sentinelUniqueGORM{ID: "f1", AccountID: "t1", Ref: "r1"}).Error; err != nil {
		t.Fatalf("create f1: %v", err)
	}
	// Two LIVE rows: both soft_delete_key="" → conflict.
	if err := db.Create(&sentinelUniqueGORM{ID: "f1b", AccountID: "t1", Ref: "r1"}).Error; err == nil {
		t.Fatal("two live rows sharing ref must violate the composite unique index")
	}
	// Soft-delete f1, stamping its id into soft_delete_key in one update (mirrors
	// the generated Delete).
	if err := db.Model(&sentinelUniqueGORM{}).Where("id = ?", "f1").
		Updates(map[string]interface{}{"deleted_at": time.Now().UTC(), "soft_delete_key": "f1"}).Error; err != nil {
		t.Fatalf("soft-delete f1: %v", err)
	}
	// Re-create ref=r1 (soft_delete_key="") → distinct from tombstoned (t1,r1,"f1").
	if err := db.Create(&sentinelUniqueGORM{ID: "f2", AccountID: "t1", Ref: "r1"}).Error; err != nil {
		t.Fatalf("re-create ref=r1 after soft-delete must succeed (sentinel): %v", err)
	}
}
