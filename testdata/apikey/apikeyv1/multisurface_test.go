package apikeyv1_test

// multisurface_test.go — F027 Phase 5b / WS-005 acceptance (AC-004): two messages
// backed by ONE storage model round-trip on BOTH backends. APIKey is the owner;
// APIKeySummary is a read projection (surface) with
// (infoblox.storage.v1.model)="APIKey" — it has no table of its own, so it reads
// rows written through the owner repository, projecting the non-secret subset
// {id, account_id, key_prefix, label} (never the key_value secret). This file
// proves the generated surface adapter works against a real SQLite database on
// both the ent and GORM backends.

import (
	"testing"

	_ "modernc.org/sqlite" // register the SQLite driver for enttest

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// TestMultiSurface_ENT: write an APIKey via the owner ent repository, then read it
// back through the APIKeySummary SURFACE ent repository (which operates over the
// owner's ent type) and confirm the projection.
func TestMultiSurface_ENT(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:multisurface_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := tenantCtx("acme")
	enc := secret.NewDev(make([]byte, 32))
	owner := apikeyv1.NewAPIKeyEntRepository(client, enc)
	// The surface constructor takes no Encryptor — it projects no secret field.
	summaries := apikeyv1.NewAPIKeySummaryEntRepository(client)

	if _, err := owner.Create(ctx, &apikeyv1.APIKey{
		Id:        "k1",
		AccountId: "acme",
		KeyValue:  "sk_secret_value",
		KeyPrefix: "sk_sec",
		Label:     "primary",
	}); err != nil {
		t.Fatalf("owner create: %v", err)
	}

	got, err := summaries.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("surface get: %v", err)
	}
	if got.Id != "k1" || got.AccountId != "acme" || got.KeyPrefix != "sk_sec" || got.Label != "primary" {
		t.Errorf("surface projection = %+v, want id=k1 account=acme prefix=sk_sec label=primary", got)
	}

	// List through the surface returns the same projected row, tenant-scoped.
	list, _, err := summaries.List(ctx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("surface list: %v", err)
	}
	if len(list) != 1 || list[0].Id != "k1" {
		t.Fatalf("surface list = %v, want exactly [k1]", list)
	}

	// A different tenant must not see acme's row through the surface (the owner's
	// ent tenant interceptor scopes the surface's reads too).
	if other, _, err := summaries.List(tenantCtx("globex"), persistence.ListOptions{}); err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant surface list = %v (err %v), want empty", other, err)
	}
}

// TestMultiSurface_GORM: the same round-trip on the GORM backend — owner write,
// surface read — proving cross-backend parity (G-005) for the multi-surface path.
func TestMultiSurface_GORM(t *testing.T) {
	db, err := gorm.Open(openTestSQLite("file:multisurface_gorm?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open gorm db: %v", err)
	}
	if err := db.AutoMigrate(&apikeyv1.APIKeyModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	ctx := tenantCtx("acme")
	enc := secret.NewDev(make([]byte, 32))
	owner := apikeyv1.NewAPIKeyRepository(db, enc)
	summaries := apikeyv1.NewAPIKeySummaryRepository(db)

	if _, err := owner.Create(ctx, &apikeyv1.APIKey{
		Id:        "k1",
		AccountId: "acme",
		KeyValue:  "sk_secret_value",
		KeyPrefix: "sk_sec",
		Label:     "primary",
	}); err != nil {
		t.Fatalf("owner create: %v", err)
	}

	got, err := summaries.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("surface get: %v", err)
	}
	if got.Id != "k1" || got.AccountId != "acme" || got.KeyPrefix != "sk_sec" || got.Label != "primary" {
		t.Errorf("surface projection = %+v, want id=k1 account=acme prefix=sk_sec label=primary", got)
	}

	list, _, err := summaries.List(ctx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("surface list: %v", err)
	}
	if len(list) != 1 || list[0].Id != "k1" {
		t.Fatalf("surface list = %v, want exactly [k1]", list)
	}
}
