package apikeyv1_test

// batch_sqlite_test.go — GORM integration tests for F026 batch methods
// (BatchGet / BatchUpdate / BatchDelete) on the generated APIKeyRepository.
// Verifies atomicity, soft-delete awareness, and tenant scoping against a real
// SQLite database. AC-010, AC-012, AC-013, AC-014, AC-015.

import (
	"context"
	"testing"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
)

// newBatchRepo opens a fresh SQLite-backed APIKeyRepository (which must satisfy
// BatchRepository — AC-010) scoped to tenant1.
func newBatchRepo(t *testing.T, dsn string) (persistence.BatchRepository[*apikeyv1.APIKey, string], context.Context) {
	t.Helper()
	db := openSoftDeleteDB(t, dsn)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	repo := apikeyv1.NewAPIKeyRepository(db, secret.NewDev(make([]byte, 32)))
	ctx := middleware.WithTenantID(context.Background(), "tenant1")
	return repo, ctx
}

func mustCreate(t *testing.T, repo persistence.BatchRepository[*apikeyv1.APIKey, string], ctx context.Context, id, label string) {
	t.Helper()
	if _, err := repo.Create(ctx, &apikeyv1.APIKey{Id: id, AccountId: "tenant1", Label: label, KeyValue: "sk_" + id}); err != nil {
		t.Fatalf("Create %s: %v", id, err)
	}
}

func TestBatch_GORM_HappyPath(t *testing.T) {
	repo, ctx := newBatchRepo(t, "batch_happy")
	mustCreate(t, repo, ctx, "a", "alpha")
	mustCreate(t, repo, ctx, "b", "beta")

	// BatchGet returns both in request order.
	got, err := repo.BatchGet(ctx, []string{"a", "b"})
	if err != nil {
		t.Fatalf("BatchGet: %v", err)
	}
	if len(got) != 2 || got[0].Id != "a" || got[1].Id != "b" {
		t.Fatalf("BatchGet order/content: %+v", got)
	}

	// BatchUpdate (field mask = label) updates both, returns in order.
	updated, err := repo.BatchUpdate(ctx, []persistence.BatchUpdateItem[*apikeyv1.APIKey, string]{
		{Key: "a", Entity: &apikeyv1.APIKey{Id: "a", Label: "alpha2"}, FieldMask: []string{"label"}},
		{Key: "b", Entity: &apikeyv1.APIKey{Id: "b", Label: "beta2"}, FieldMask: []string{"label"}},
	})
	if err != nil {
		t.Fatalf("BatchUpdate: %v", err)
	}
	if len(updated) != 2 || updated[0].Label != "alpha2" || updated[1].Label != "beta2" {
		t.Fatalf("BatchUpdate result: %+v", updated)
	}
	if a, _ := repo.Get(ctx, "a"); a.Label != "alpha2" {
		t.Fatalf("Get after BatchUpdate: want alpha2, got %q", a.Label)
	}

	// BatchDelete soft-deletes both; subsequent Get → NotFound.
	if err := repo.BatchDelete(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("BatchDelete: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := repo.Get(ctx, id); err != persistence.ErrNotFound {
			t.Fatalf("Get %s after BatchDelete: want ErrNotFound, got %v", id, err)
		}
	}
}

func TestBatch_GORM_AtomicMissingKey(t *testing.T) {
	repo, ctx := newBatchRepo(t, "batch_atomic")
	mustCreate(t, repo, ctx, "a", "alpha")

	// BatchUpdate with one missing key rolls back: "a" must be unchanged.
	_, err := repo.BatchUpdate(ctx, []persistence.BatchUpdateItem[*apikeyv1.APIKey, string]{
		{Key: "a", Entity: &apikeyv1.APIKey{Id: "a", Label: "changed"}, FieldMask: []string{"label"}},
		{Key: "missing", Entity: &apikeyv1.APIKey{Id: "missing", Label: "x"}, FieldMask: []string{"label"}},
	})
	if err != persistence.ErrNotFound {
		t.Fatalf("BatchUpdate missing: want ErrNotFound, got %v", err)
	}
	if a, _ := repo.Get(ctx, "a"); a.Label != "alpha" {
		t.Fatalf("BatchUpdate not atomic: a.Label = %q, want alpha", a.Label)
	}

	// BatchDelete with one missing key rolls back: "a" must still be live.
	if err := repo.BatchDelete(ctx, []string{"a", "missing"}); err != persistence.ErrNotFound {
		t.Fatalf("BatchDelete missing: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Get(ctx, "a"); err != nil {
		t.Fatalf("BatchDelete not atomic: a should be live, got %v", err)
	}
}

func TestBatch_GORM_SoftDeletedKey(t *testing.T) {
	repo, ctx := newBatchRepo(t, "batch_softdel")
	mustCreate(t, repo, ctx, "a", "alpha")
	mustCreate(t, repo, ctx, "b", "beta")
	if err := repo.Delete(ctx, "b"); err != nil {
		t.Fatalf("Delete b: %v", err)
	}

	// BatchGet treats a soft-deleted key as not found.
	if _, err := repo.BatchGet(ctx, []string{"a", "b"}); err != persistence.ErrNotFound {
		t.Fatalf("BatchGet soft-deleted: want ErrNotFound, got %v", err)
	}
	// BatchDelete on an already-soft-deleted key is atomic NotFound; "a" stays live.
	if err := repo.BatchDelete(ctx, []string{"a", "b"}); err != persistence.ErrNotFound {
		t.Fatalf("BatchDelete soft-deleted: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Get(ctx, "a"); err != nil {
		t.Fatalf("BatchDelete soft-deleted not atomic: a should be live, got %v", err)
	}
}

func TestBatch_GORM_TenantScoped(t *testing.T) {
	repo, ctx := newBatchRepo(t, "batch_tenant")
	mustCreate(t, repo, ctx, "a", "alpha") // tenant1

	// Create a key under tenant2.
	ctx2 := middleware.WithTenantID(context.Background(), "tenant2")
	if _, err := repo.Create(ctx2, &apikeyv1.APIKey{Id: "t2", AccountId: "tenant2", Label: "other", KeyValue: "sk_t2"}); err != nil {
		t.Fatalf("Create tenant2 key: %v", err)
	}

	// From tenant1, the tenant2 key is invisible: BatchGet → NotFound.
	if _, err := repo.BatchGet(ctx, []string{"a", "t2"}); err != persistence.ErrNotFound {
		t.Fatalf("cross-tenant BatchGet: want ErrNotFound, got %v", err)
	}
	// From tenant1, BatchDelete of the tenant2 key is NotFound and does not delete it.
	if err := repo.BatchDelete(ctx, []string{"t2"}); err != persistence.ErrNotFound {
		t.Fatalf("cross-tenant BatchDelete: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Get(ctx2, "t2"); err != nil {
		t.Fatalf("tenant2 key must survive cross-tenant BatchDelete, got %v", err)
	}
}
