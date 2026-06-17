package apikeyv1_test

// ent_tenant_mutation_test.go — cross-tenant isolation for ent MUTATIONS.
// ent query interceptors (TenantMixin) scope reads automatically, but they do
// NOT run for mutations, so Update_/Delete_ must carry an explicit tenant
// predicate or a caller could mutate another tenant's row by ID.

import (
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

func TestEntRepository_CrossTenantMutationDenied(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:xtenant_mut?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))

	alice := tenantCtx("alice")
	bob := tenantCtx("bob")
	if _, err := repo.Create(alice, &apikeyv1.APIKey{Id: "a1", Name: "a", AccountId: "alice", KeyPrefix: "pa", KeyValue: "sk_a1"}); err != nil {
		t.Fatalf("create alice key: %v", err)
	}

	// Bob must NOT be able to delete alice's key by ID.
	if err := repo.Delete(bob, "a1"); err != persistence.ErrNotFound {
		t.Errorf("cross-tenant Delete: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Get(alice, "a1"); err != nil {
		t.Errorf("alice's key must survive bob's delete attempt, got %v", err)
	}

	// Bob must NOT be able to update alice's key by ID.
	if _, err := repo.Update(bob, "a1", &apikeyv1.APIKey{Id: "a1", KeyPrefix: "HACKED"}, "key_prefix"); err != persistence.ErrNotFound {
		t.Errorf("cross-tenant Update: want ErrNotFound, got %v", err)
	}
	if got, err := repo.Get(alice, "a1"); err != nil || got.KeyPrefix != "pa" {
		t.Errorf("alice's key_prefix must be unchanged by bob: got=%+v err=%v", got, err)
	}
}
