package apikeyv1_test

import (
	"context"
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// TestRepoCustomHooks verifies the F027 owned override seam: registering the
// exported FromEnt*Custom / ToEnt*OnCreate hooks makes the generated adapter run
// them — the read hook after the deterministic projection, the write hook just
// before save. The hooks are package vars set from this (external) package, then
// reset, proving a consumer can register them from their own code.
func TestRepoCustomHooks(t *testing.T) {
	apikeyv1.FromEntAPIKeyCustom = func(e *ent.APIKey, p *apikeyv1.APIKey) {
		p.Label = "derived:" + e.ID // a computed value the generator can't derive
	}
	createHookRan := false
	apikeyv1.ToEntAPIKeyOnCreate = func(p *apikeyv1.APIKey, b *ent.APIKeyCreate) {
		createHookRan = true
	}
	defer func() {
		apikeyv1.FromEntAPIKeyCustom = nil
		apikeyv1.ToEntAPIKeyOnCreate = nil
	}()

	client := enttest.Open(t, "sqlite3", "file:custom_hook?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))
	ctx := middleware.WithTenantID(context.Background(), "t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "k1", AccountId: "t1", KeyValue: "sk_1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !createHookRan {
		t.Error("ToEntAPIKeyOnCreate hook did not run on Create")
	}
	if created.Label != "derived:k1" {
		t.Errorf("FromEntAPIKeyCustom not applied on Create projection: Label=%q", created.Label)
	}

	got, err := repo.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "derived:k1" {
		t.Errorf("FromEntAPIKeyCustom not applied on Get projection: Label=%q", got.Label)
	}
}
