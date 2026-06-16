package apikeyv1_test

// update_zerovalue_sqlite_test.go — regression test for the generated GORM
// Update dropping zero-value fields (devedge-assessment-2026-06-16, issue 011).
//
// Before the fix, the no-field-mask Update path called q.Updates(struct), and
// GORM skips zero-valued struct fields, so an update that set a string to "",
// a bool to false, or a number to 0 was silently lost while the API reported
// success. The fix updates via a column map (which persists zero values) and
// only rewrites the secret columns when a new secret value is supplied.

import (
	"context"
	"testing"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
)

// TestUpdate_GORM_PersistsZeroValue_NoFieldMask verifies that a full update
// (no field mask) which clears a string column to "" actually persists, and
// that it does NOT wipe the stored secret (no key_value supplied).
func TestUpdate_GORM_PersistsZeroValue_NoFieldMask(t *testing.T) {
	db := openSoftDeleteDB(t, "update_zerovalue_nomask")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := middleware.WithTenantID(context.Background(), "tenant1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{
		Id:        "zv-key-1",
		AccountId: "tenant1",
		KeyPrefix: "sk_ab",
		Label:     "initial-label",
		KeyValue:  "sk_secret_xyz",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Label != "initial-label" {
		t.Fatalf("Create: want Label=initial-label, got %q", created.Label)
	}

	// Full update (no field mask) that clears Label to the zero value while
	// keeping KeyPrefix and supplying NO new secret.
	updated, err := repo.Update(ctx, "zv-key-1", &apikeyv1.APIKey{
		Id:        "zv-key-1",
		AccountId: "tenant1",
		KeyPrefix: "sk_ab",
		Label:     "", // zero value — must persist
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Label != "" {
		t.Errorf("Update returned Label=%q, want empty (zero value dropped)", updated.Label)
	}

	// Re-read to confirm the empty value is what's actually stored.
	got, err := repo.Get(ctx, "zv-key-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Label != "" {
		t.Errorf("Get after zero-value Update: Label=%q, want empty — the update was silently dropped", got.Label)
	}
	if got.KeyPrefix != "sk_ab" {
		t.Errorf("Get after Update: KeyPrefix=%q, want sk_ab (non-zero field clobbered)", got.KeyPrefix)
	}

	// The secret must survive an update that did not supply a new key_value.
	var model apikeyv1.APIKeyModel
	if err := db.Where("id = ?", "zv-key-1").First(&model).Error; err != nil {
		t.Fatalf("read model row: %v", err)
	}
	if model.KeyValueCipher == "" || model.KeyValueHash == "" {
		t.Error("secret columns were wiped by a non-secret update (cipher/hash empty)")
	}
}

// TestUpdate_GORM_PersistsZeroValue_FieldMask verifies the field-mask path also
// persists a zero value for the masked column (GORM Select forces the write).
func TestUpdate_GORM_PersistsZeroValue_FieldMask(t *testing.T) {
	db := openSoftDeleteDB(t, "update_zerovalue_mask")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := middleware.WithTenantID(context.Background(), "tenant1")

	if _, err := repo.Create(ctx, &apikeyv1.APIKey{
		Id:        "zv-key-2",
		AccountId: "tenant1",
		Label:     "initial-label",
		KeyValue:  "sk_secret_xyz",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update only "label" to the zero value via a field mask.
	if _, err := repo.Update(ctx, "zv-key-2", &apikeyv1.APIKey{
		Id:    "zv-key-2",
		Label: "",
	}, "label"); err != nil {
		t.Fatalf("Update with mask: %v", err)
	}

	got, err := repo.Get(ctx, "zv-key-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Label != "" {
		t.Errorf("Get after masked zero-value Update: Label=%q, want empty", got.Label)
	}
}
