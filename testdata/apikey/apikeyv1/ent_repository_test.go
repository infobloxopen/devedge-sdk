package apikeyv1_test

import (
	"context"
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// tenantCtx creates a context with accountID injected as the tenant identity.
func tenantCtx(accountID string) context.Context {
	return middleware.WithTenantID(context.Background(), accountID)
}

// TestEntRepository_TenantIsolation verifies that each tenant only sees its own
// keys — a cross-tenant Get returns ErrNotFound and List returns only own rows.
func TestEntRepository_TenantIsolation(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:tenant_iso?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	key := make([]byte, 32)
	enc := secret.NewDev(key)
	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)

	// Create alice's key under alice's tenant context.
	aliceCtx := tenantCtx("alice")
	aliceKey := &apikeyv1.APIKey{
		Id:        "alice-1",
		Name:      "alice key",
		AccountId: "alice",
		KeyValue:  "sk_alice_abc",
	}
	created, err := repo.Create(aliceCtx, aliceKey)
	if err != nil {
		t.Fatalf("create alice key: %v", err)
	}
	if created.KeyValue != "" {
		t.Error("KeyValue must be cleared in Create response")
	}

	// Create bob's key under bob's tenant context.
	bobCtx := tenantCtx("bob")
	bobKey := &apikeyv1.APIKey{
		Id:        "bob-1",
		Name:      "bob key",
		AccountId: "bob",
		KeyValue:  "sk_bob_xyz",
	}
	if _, err := repo.Create(bobCtx, bobKey); err != nil {
		t.Fatalf("create bob key: %v", err)
	}

	// Alice lists — TenantMixin scopes to alice; only her key is returned.
	aliceKeys, _, err := repo.List(aliceCtx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(aliceKeys) != 1 {
		t.Fatalf("alice list: expected 1 key, got %d", len(aliceKeys))
	}
	if aliceKeys[0].AccountId != "alice" {
		t.Errorf("alice list returned wrong account: %s", aliceKeys[0].AccountId)
	}

	// Bob lists — only bob's key is returned.
	bobKeys, _, err := repo.List(bobCtx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	if len(bobKeys) != 1 {
		t.Fatalf("bob list: expected 1 key, got %d", len(bobKeys))
	}
	if bobKeys[0].AccountId != "bob" {
		t.Errorf("bob list returned wrong account: %s", bobKeys[0].AccountId)
	}

	// Bob tries to Get alice's key — TenantMixin scopes the query, so not found.
	_, err = repo.Get(bobCtx, "alice-1")
	if err != persistence.ErrNotFound {
		t.Errorf("bob get alice's key: expected ErrNotFound, got %v", err)
	}
}

// TestEntRepository_SecretFieldStoredAsHash verifies that KeyValue is hashed and
// encrypted on Create and is never returned in the response.
func TestEntRepository_SecretFieldStoredAsHash(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:secret_hash?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	key := make([]byte, 32)
	key[0] = 1
	enc := secret.NewDev(key)
	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)

	ctx := tenantCtx("tenant1")
	k := &apikeyv1.APIKey{
		Id:        "k1",
		Name:      "test key",
		AccountId: "tenant1",
		KeyValue:  "sk_live_abc123",
	}
	result, err := repo.Create(ctx, k)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result.KeyValue != "" {
		t.Error("KeyValue must be cleared in Create response — must not echo secret back")
	}
	if result.Id != "k1" {
		t.Errorf("id mismatch: got %q", result.Id)
	}
}

// TestEntRepository_GetAndDelete exercises basic Get and Delete lifecycle.
func TestEntRepository_GetAndDelete(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:get_delete?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)

	ctx := tenantCtx("t1")
	_, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "x1", Name: "x", AccountId: "t1", KeyValue: "val"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(ctx, "x1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Id != "x1" {
		t.Errorf("get id: want x1, got %s", got.Id)
	}

	if err := repo.Delete(ctx, "x1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = repo.Get(ctx, "x1")
	if err != persistence.ErrNotFound {
		t.Errorf("get after delete: expected ErrNotFound, got %v", err)
	}
}

// TestEntRepository_ETagStampedAndChanges is the ent-backend counterpart of
// TestETag_GORM_StampedAndChanges (#49): EtagMixin must stamp a non-empty token
// on Create, surface it on read (so a client can echo it as If-Match), keep it
// stable between writes, and re-stamp it on Update (so a stale If-Match yields a
// 412). Before the fix protoc-gen-ent dropped the etag field entirely, so the
// ent ETag was always empty and the documented conditional-request loop was a
// dead end on ent while working on GORM.
func TestEntRepository_ETagStampedAndChanges(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:etag_ent_rt?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)
	ctx := tenantCtx("t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "k1", Name: "a", AccountId: "t1", KeyValue: "sk_1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Etag == "" {
		t.Fatal("ent Create response ETag is empty; want an EtagMixin-stamped token")
	}

	got, err := repo.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Etag != created.Etag {
		t.Errorf("Get ETag = %q, want %q (must be stable until the next write)", got.Etag, created.Etag)
	}

	updated, err := repo.Update(ctx, "k1", &apikeyv1.APIKey{Id: "k1", Name: "b"}, "name")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Etag == "" || updated.Etag == created.Etag {
		t.Errorf("Update ETag = %q, want a fresh non-empty token (was %q)", updated.Etag, created.Etag)
	}
}
