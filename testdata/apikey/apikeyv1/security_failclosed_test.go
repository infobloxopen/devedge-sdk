package apikeyv1_test

// security_failclosed_test.go — SEC-001 (fail-closed fence), SEC-002 (verified
// principal is the tenant authority), and SEC-003 (page-size clamp) regressions
// on the ent- and GORM-backed APIKey repositories. Each test FAILS on the old
// fail-open code and PASSES on the fail-closed fence.
//
// Helpers reused from this package: tenantCtx (ent_repository_test.go),
// openTestSQLite (security_isolation_test.go).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// wantPermissionDenied asserts err is a gRPC PermissionDenied status.
func wantPermissionDenied(t *testing.T, op string, err error) {
	t.Helper()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("%s: want PermissionDenied (fail closed), got %v", op, err)
	}
}

// newGormAPIKeyRepo opens a fresh in-memory GORM APIKey repository.
func newGormAPIKeyRepo(t *testing.T, dsn string) *apikeyv1.APIKeyRepository {
	t.Helper()
	db, err := gorm.Open(openTestSQLite(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open gorm db: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&apikeyv1.APIKeyModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return apikeyv1.NewAPIKeyRepository(db, secret.NewDev(make([]byte, 32)))
}

// ---- SEC-001: fail closed on an absent tenant ----

func TestSecurity_FailClosed_NoTenant_Ent(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fc_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))

	// Seed one row under alice so Get/Update/Delete have a target.
	if _, err := repo.Create(tenantCtx("alice"), &apikeyv1.APIKey{Id: "fc-1", AccountId: "alice"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	bare := context.Background() // no principal, no header, no system context
	_, err := repo.Create(bare, &apikeyv1.APIKey{Id: "fc-2", AccountId: "alice"})
	wantPermissionDenied(t, "Create", err)
	_, err = repo.Get(bare, "fc-1")
	wantPermissionDenied(t, "Get", err)
	_, _, err = repo.List(bare, persistence.ListOptions{})
	wantPermissionDenied(t, "List", err)
	_, err = repo.Update(bare, "fc-1", &apikeyv1.APIKey{Id: "fc-1", Label: "x"})
	wantPermissionDenied(t, "Update", err)
	err = repo.Delete(bare, "fc-1")
	wantPermissionDenied(t, "Delete", err)
}

func TestSecurity_FailClosed_NoTenant_GORM(t *testing.T) {
	repo := newGormAPIKeyRepo(t, "file:fc_gorm?mode=memory&cache=shared")

	if _, err := repo.Create(tenantCtx("alice"), &apikeyv1.APIKey{Id: "fc-1", AccountId: "alice"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	bare := context.Background()
	_, err := repo.Create(bare, &apikeyv1.APIKey{Id: "fc-2", AccountId: "alice"})
	wantPermissionDenied(t, "Create", err)
	_, err = repo.Get(bare, "fc-1")
	wantPermissionDenied(t, "Get", err)
	_, _, err = repo.List(bare, persistence.ListOptions{})
	wantPermissionDenied(t, "List", err)
	_, err = repo.Update(bare, "fc-1", &apikeyv1.APIKey{Id: "fc-1", Label: "x"})
	wantPermissionDenied(t, "Update", err)
	err = repo.Delete(bare, "fc-1")
	wantPermissionDenied(t, "Delete", err)
}

// ---- SEC-001: WithSystemContext bypasses the fence (the sanctioned opt-out) ----

func TestSecurity_SystemContextBypass_Ent(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:sys_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))

	if _, err := repo.Create(tenantCtx("alice"), &apikeyv1.APIKey{Id: "a1", AccountId: "alice"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := repo.Create(tenantCtx("bob"), &apikeyv1.APIKey{Id: "b1", AccountId: "bob"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	sys := middleware.WithSystemContext(context.Background())
	items, _, err := repo.List(sys, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("system List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("system context must see all tenants' rows: want 2, got %d", len(items))
	}
	if _, err := repo.Get(sys, "b1"); err != nil {
		t.Fatalf("system Get across tenant must succeed, got %v", err)
	}
}

func TestSecurity_SystemContextBypass_GORM(t *testing.T) {
	repo := newGormAPIKeyRepo(t, "file:sys_gorm?mode=memory&cache=shared")

	if _, err := repo.Create(tenantCtx("alice"), &apikeyv1.APIKey{Id: "a1", AccountId: "alice"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := repo.Create(tenantCtx("bob"), &apikeyv1.APIKey{Id: "b1", AccountId: "bob"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	sys := middleware.WithSystemContext(context.Background())
	items, _, err := repo.List(sys, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("system List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("system context must see all tenants' rows: want 2, got %d", len(items))
	}
	if _, err := repo.Get(sys, "b1"); err != nil {
		t.Fatalf("system Get across tenant must succeed, got %v", err)
	}
}

// ---- SEC-002: the VERIFIED PRINCIPAL is the tenant authority, not the header ----

// principalCtx puts a verified principal (authoritative) plus a DIFFERENT stashed
// account-id header (the untrusted routing hint) on the context.
func principalCtx(principalTenant, headerTenant string) context.Context {
	ctx := middleware.WithTenantID(context.Background(), headerTenant)
	return middleware.WithPrincipal(ctx, authz.Principal{Subject: "u", Tenant: principalTenant})
}

func TestSecurity_PrincipalIsTenantAuthority_Ent(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:princ_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))

	if _, err := repo.Create(tenantCtx("alice"), &apikeyv1.APIKey{Id: "alice-key", AccountId: "alice"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := repo.Create(tenantCtx("bob"), &apikeyv1.APIKey{Id: "bob-key", AccountId: "bob"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	// principal=alice, spoofed header=bob. The fence must scope to the principal (alice).
	ctx := principalCtx("alice", "bob")
	if _, err := repo.Get(ctx, "alice-key"); err != nil {
		t.Fatalf("principal 'alice' must reach alice's row despite header 'bob', got %v", err)
	}
	if _, err := repo.Get(ctx, "bob-key"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("principal 'alice' must NOT reach bob's row via a spoofed header, got %v", err)
	}
}

func TestSecurity_PrincipalIsTenantAuthority_GORM(t *testing.T) {
	repo := newGormAPIKeyRepo(t, "file:princ_gorm?mode=memory&cache=shared")

	if _, err := repo.Create(tenantCtx("alice"), &apikeyv1.APIKey{Id: "alice-key", AccountId: "alice"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := repo.Create(tenantCtx("bob"), &apikeyv1.APIKey{Id: "bob-key", AccountId: "bob"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	ctx := principalCtx("alice", "bob")
	if _, err := repo.Get(ctx, "alice-key"); err != nil {
		t.Fatalf("principal 'alice' must reach alice's row despite header 'bob', got %v", err)
	}
	if _, err := repo.Get(ctx, "bob-key"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("principal 'alice' must NOT reach bob's row via a spoofed header, got %v", err)
	}
}

// ---- SEC-003: List clamps page_size to persistence.MaxPageSize ----

func TestList_PageSizeClamp_Ent(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:clamp_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))

	ctx := tenantCtx("alice")
	for i := 0; i < persistence.MaxPageSize+1; i++ {
		if _, err := repo.Create(ctx, &apikeyv1.APIKey{Id: fmt.Sprintf("k-%04d", i), AccountId: "alice"}); err != nil {
			t.Fatalf("seed create %d: %v", i, err)
		}
	}

	items, next, err := repo.List(ctx, persistence.ListOptions{PageSize: 100000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != persistence.MaxPageSize {
		t.Fatalf("page size must clamp to MaxPageSize=%d, got %d", persistence.MaxPageSize, len(items))
	}
	if next == "" {
		t.Fatal("a clamped List over MaxPageSize+1 rows must return a next page token")
	}
}
