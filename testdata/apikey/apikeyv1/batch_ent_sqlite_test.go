package apikeyv1_test

// batch_ent_sqlite_test.go — ent integration tests for F026 batch methods on the
// generated APIKeyEntRepository wrapper. Verifies atomicity, soft-delete, and
// tenant scoping (mutations carry explicit predicates since ent interceptors do
// not cover mutations). AC-011, AC-014, AC-015, AC-020.

import (
	"context"
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

func newEntBatchRepo(t *testing.T, dsn string) *apikeyv1.APIKeyEntRepository {
	t.Helper()
	client := enttest.Open(t, "sqlite3", "file:"+dsn+"?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	t.Cleanup(func() { client.Close() })
	// The wrapper must satisfy BatchRepository (AC-011, enforced at compile time).
	var _ persistence.BatchRepository[*apikeyv1.APIKey, string] = apikeyv1.NewAPIKeyEntBatchRepository(client, secret.NewDev(make([]byte, 32)))
	return apikeyv1.NewAPIKeyEntBatchRepository(client, secret.NewDev(make([]byte, 32)))
}

func entCreate(t *testing.T, repo *apikeyv1.APIKeyEntRepository, ctx context.Context, account, id, prefix string) {
	t.Helper()
	if _, err := repo.Create(ctx, &apikeyv1.APIKey{Id: id, Name: id, AccountId: account, KeyPrefix: prefix, KeyValue: "sk_" + id}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func TestEntBatch_HappyPath(t *testing.T) {
	repo := newEntBatchRepo(t, "ent_batch_happy")
	ctx := tenantCtx("alice")
	entCreate(t, repo, ctx, "alice", "a", "pa")
	entCreate(t, repo, ctx, "alice", "b", "pb")

	// BatchGet returns both in request order.
	got, err := repo.BatchGet(ctx, []string{"a", "b"})
	if err != nil {
		t.Fatalf("BatchGet: %v", err)
	}
	if len(got) != 2 || got[0].Id != "a" || got[1].Id != "b" {
		t.Fatalf("BatchGet order/content: %+v", got)
	}

	// BatchUpdate (mask = key_prefix) updates both, returns in order.
	updated, err := repo.BatchUpdate(ctx, []persistence.BatchUpdateItem[*apikeyv1.APIKey, string]{
		{Key: "a", Entity: &apikeyv1.APIKey{Id: "a", KeyPrefix: "pa2"}, FieldMask: []string{"key_prefix"}},
		{Key: "b", Entity: &apikeyv1.APIKey{Id: "b", KeyPrefix: "pb2"}, FieldMask: []string{"key_prefix"}},
	})
	if err != nil {
		t.Fatalf("BatchUpdate: %v", err)
	}
	if len(updated) != 2 || updated[0].KeyPrefix != "pa2" || updated[1].KeyPrefix != "pb2" {
		t.Fatalf("BatchUpdate result: %+v", updated)
	}
	if a, _ := repo.Get(ctx, "a"); a.KeyPrefix != "pa2" {
		t.Fatalf("Get after BatchUpdate: want pa2, got %q", a.KeyPrefix)
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

func TestEntBatch_AtomicMissingKey(t *testing.T) {
	repo := newEntBatchRepo(t, "ent_batch_atomic")
	ctx := tenantCtx("alice")
	entCreate(t, repo, ctx, "alice", "a", "pa")

	// BatchUpdate with a missing key rolls back: "a" must be unchanged.
	_, err := repo.BatchUpdate(ctx, []persistence.BatchUpdateItem[*apikeyv1.APIKey, string]{
		{Key: "a", Entity: &apikeyv1.APIKey{Id: "a", KeyPrefix: "changed"}, FieldMask: []string{"key_prefix"}},
		{Key: "missing", Entity: &apikeyv1.APIKey{Id: "missing", KeyPrefix: "x"}, FieldMask: []string{"key_prefix"}},
	})
	if err != persistence.ErrNotFound {
		t.Fatalf("BatchUpdate missing: want ErrNotFound, got %v", err)
	}
	if a, _ := repo.Get(ctx, "a"); a.KeyPrefix != "pa" {
		t.Fatalf("BatchUpdate not atomic: a.KeyPrefix = %q, want pa", a.KeyPrefix)
	}

	// BatchDelete with a missing key rolls back: "a" must still be live.
	if err := repo.BatchDelete(ctx, []string{"a", "missing"}); err != persistence.ErrNotFound {
		t.Fatalf("BatchDelete missing: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Get(ctx, "a"); err != nil {
		t.Fatalf("BatchDelete not atomic: a should be live, got %v", err)
	}
}

func TestEntBatch_SoftDeletedKey(t *testing.T) {
	repo := newEntBatchRepo(t, "ent_batch_softdel")
	ctx := tenantCtx("alice")
	entCreate(t, repo, ctx, "alice", "a", "pa")
	entCreate(t, repo, ctx, "alice", "b", "pb")
	if err := repo.Delete(ctx, "b"); err != nil {
		t.Fatalf("Delete b: %v", err)
	}

	// BatchGet treats a soft-deleted key as not found (rides the interceptor).
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

func TestEntBatch_TenantScoped(t *testing.T) {
	repo := newEntBatchRepo(t, "ent_batch_tenant")
	alice := tenantCtx("alice")
	bob := tenantCtx("bob")
	entCreate(t, repo, alice, "alice", "a", "pa")
	entCreate(t, repo, bob, "bob", "b2", "pb2")

	// From alice, bob's key is invisible: BatchGet → NotFound.
	if _, err := repo.BatchGet(alice, []string{"a", "b2"}); err != persistence.ErrNotFound {
		t.Fatalf("cross-tenant BatchGet: want ErrNotFound, got %v", err)
	}
	// From alice, BatchDelete of bob's key is NotFound and must not delete it.
	if err := repo.BatchDelete(alice, []string{"b2"}); err != persistence.ErrNotFound {
		t.Fatalf("cross-tenant BatchDelete: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Get(bob, "b2"); err != nil {
		t.Fatalf("bob key must survive cross-tenant BatchDelete, got %v", err)
	}
}
