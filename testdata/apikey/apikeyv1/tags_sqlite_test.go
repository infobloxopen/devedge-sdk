package apikeyv1_test

// tags_sqlite_test.go — end-to-end persistence tests for the Tags field kind
// (a proto map<string,string>), exercising both storage backends:
//   - the generated GORM repository, where tags is a types.Tags JSONB column;
//   - the ent-backed repository, where tags is a JSON field.
// Both use the same inline pure-Go SQLite database as the other *_sqlite tests.

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// TestTags_GORM_RoundTrip verifies a map<string,string> tags field round-trips
// through the generated GORM repository: stored as a JSONB column on Create,
// surfaced on Get, replaced by a full Update, and stored as NULL (read back
// empty) when absent.
func TestTags_GORM_RoundTrip(t *testing.T) {
	db := openSoftDeleteDB(t, "tags_gorm")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := middleware.WithTenantID(context.Background(), "t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{
		Id:        "k1",
		AccountId: "t1",
		KeyValue:  "sk_1",
		Tags:      map[string]string{"env": "prod", "team": "platform"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Tags["env"] != "prod" || created.Tags["team"] != "platform" {
		t.Fatalf("create tags = %v, want env=prod team=platform", created.Tags)
	}

	got, err := repo.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Tags) != 2 || got.Tags["env"] != "prod" || got.Tags["team"] != "platform" {
		t.Fatalf("get tags = %v, want the JSONB-persisted map round-tripped", got.Tags)
	}

	// A full (no field mask) update replaces the whole tags map.
	if _, err := repo.Update(ctx, "k1", &apikeyv1.APIKey{
		Id:   "k1",
		Tags: map[string]string{"env": "staging"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = repo.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if len(got.Tags) != 1 || got.Tags["env"] != "staging" {
		t.Fatalf("tags after update = %v, want {env:staging}", got.Tags)
	}

	// An APIKey created without tags stores NULL and reads back empty.
	if _, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "k2", AccountId: "t1", KeyValue: "sk_2"}); err != nil {
		t.Fatalf("create k2: %v", err)
	}
	got2, err := repo.Get(ctx, "k2")
	if err != nil {
		t.Fatalf("get k2: %v", err)
	}
	if len(got2.Tags) != 0 {
		t.Fatalf("absent tags = %v, want empty", got2.Tags)
	}
}

// TestTags_GORM_Filter verifies AIP-160 tag filtering on the generated GORM
// repository: value equality/inequality on a key, has() presence, and AND/OR/NOT
// composition — all evaluated as dialect-aware JSON SQL against SQLite.
func TestTags_GORM_Filter(t *testing.T) {
	db := openSoftDeleteDB(t, "tags_filter")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := middleware.WithTenantID(context.Background(), "t1")

	seed := []*apikeyv1.APIKey{
		{Id: "k1", AccountId: "t1", KeyValue: "s1", Tags: map[string]string{"env": "prod", "team": "platform"}},
		{Id: "k2", AccountId: "t1", KeyValue: "s2", Tags: map[string]string{"env": "staging", "team": "platform"}},
		{Id: "k3", AccountId: "t1", KeyValue: "s3", Tags: map[string]string{"env": "prod"}},
		{Id: "k4", AccountId: "t1", KeyValue: "s4"}, // no tags at all
	}
	for _, k := range seed {
		if _, err := repo.Create(ctx, k); err != nil {
			t.Fatalf("create %s: %v", k.Id, err)
		}
	}

	list := func(expr string) []string {
		ks, _, err := repo.List(ctx, persistence.ListOptions{Filter: expr, PageSize: 100})
		if err != nil {
			t.Fatalf("list %q: %v", expr, err)
		}
		out := make([]string, len(ks))
		for i, k := range ks {
			out[i] = k.Id
		}
		sort.Strings(out)
		return out
	}

	cases := []struct {
		name string
		expr string
		want []string
	}{
		{"value equality", `tags.env = "prod"`, []string{"k1", "k3"}},
		{"value inequality", `tags.env != "prod"`, []string{"k2"}},
		{"presence", `has(tags.team)`, []string{"k1", "k2"}},
		{"equality AND presence", `tags.env = "prod" AND has(tags.team)`, []string{"k1"}},
		{"equality OR", `tags.env = "staging" OR tags.env = "prod"`, []string{"k1", "k2", "k3"}},
		{"NOT presence", `NOT has(tags.team)`, []string{"k3", "k4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := list(tc.expr)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("filter %q = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}

	// An unsupported operator on a tag field is a clean InvalidArgument, not a 500.
	if _, _, err := repo.List(ctx, persistence.ListOptions{Filter: `tags.env < "x"`}); err == nil {
		t.Error("expected an error for an unsupported tag operator, got nil")
	}
}

// TestTags_Ent_RoundTrip verifies the same field round-trips through the
// ent-backed repository, where tags is an ent JSON field.
func TestTags_Ent_RoundTrip(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:tags_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)
	ctx := tenantCtx("t1")

	if _, err := repo.Create(ctx, &apikeyv1.APIKey{
		Id:        "k1",
		Name:      "k",
		AccountId: "t1",
		KeyValue:  "v",
		Tags:      map[string]string{"env": "prod", "region": "us"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Tags["env"] != "prod" || got.Tags["region"] != "us" {
		t.Fatalf("ent get tags = %v, want env=prod region=us", got.Tags)
	}

	// The ent Update_ seam is full-update style; tags are replaced.
	if _, err := repo.Update(ctx, "k1", &apikeyv1.APIKey{
		Id:   "k1",
		Name: "k",
		Tags: map[string]string{"env": "dev"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = repo.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if len(got.Tags) != 1 || got.Tags["env"] != "dev" {
		t.Fatalf("ent tags after update = %v, want {env:dev}", got.Tags)
	}

	// Sanity: cross-tenant isolation still holds with the new field present.
	if _, err := repo.Get(tenantCtx("other"), "k1"); err != persistence.ErrNotFound {
		t.Errorf("cross-tenant get: want ErrNotFound, got %v", err)
	}
}

// TestTags_Ent_Filter verifies AIP-160 tag filtering on the ent-backed
// repository, translated to dialect-aware ent predicates (sqljson) and evaluated
// against SQLite — parity with the GORM backend.
func TestTags_Ent_Filter(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:tags_ent_filter?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)
	ctx := tenantCtx("t1")

	seed := []*apikeyv1.APIKey{
		{Id: "k1", Name: "k1", AccountId: "t1", KeyValue: "s1", Tags: map[string]string{"env": "prod", "team": "platform"}},
		{Id: "k2", Name: "k2", AccountId: "t1", KeyValue: "s2", Tags: map[string]string{"env": "staging", "team": "platform"}},
		{Id: "k3", Name: "k3", AccountId: "t1", KeyValue: "s3", Tags: map[string]string{"env": "prod"}},
		{Id: "k4", Name: "k4", AccountId: "t1", KeyValue: "s4"}, // no tags
	}
	for _, k := range seed {
		if _, err := repo.Create(ctx, k); err != nil {
			t.Fatalf("create %s: %v", k.Id, err)
		}
	}

	list := func(expr string) []string {
		ks, _, err := repo.List(ctx, persistence.ListOptions{Filter: expr, PageSize: 100})
		if err != nil {
			t.Fatalf("list %q: %v", expr, err)
		}
		out := make([]string, len(ks))
		for i, k := range ks {
			out[i] = k.Id
		}
		sort.Strings(out)
		return out
	}

	cases := []struct {
		name string
		expr string
		want []string
	}{
		{"value equality", `tags.env = "prod"`, []string{"k1", "k3"}},
		{"value inequality", `tags.env != "prod"`, []string{"k2"}},
		{"presence", `has(tags.team)`, []string{"k1", "k2"}},
		{"equality AND presence", `tags.env = "prod" AND has(tags.team)`, []string{"k1"}},
		{"NOT presence", `NOT has(tags.team)`, []string{"k3", "k4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := list(tc.expr)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("filter %q = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}

	// An unsupported tag operator surfaces as a clean error from the repo.
	if _, _, err := repo.List(ctx, persistence.ListOptions{Filter: `tags.env < "x"`}); err == nil {
		t.Error("expected an error for an unsupported tag operator, got nil")
	}
}
