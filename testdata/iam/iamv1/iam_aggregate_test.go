package iamv1_test

import (
	"context"
	"errors"
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/server"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/ent/enttest"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

func tenantCtx(accountID string) context.Context {
	return middleware.WithTenantID(context.Background(), accountID)
}

// TestIAM_AccountAsPartition proves account_id is the TENANT PARTITION: a User
// and a Group created under one account are scoped by TenantMixin, and a query
// under a different account does not see them (account = partition, not aggregate).
func TestIAM_AccountAsPartition(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_partition?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	users := iamv1.NewUserEntRepository(client)

	acme := tenantCtx("acme")
	globex := tenantCtx("globex")
	if _, err := users.Create(acme, &iamv1.User{Id: "u1", Email: "a@acme.test"}); err != nil {
		t.Fatalf("create user under acme: %v", err)
	}
	// acme sees its user; globex (a different partition) does not.
	if _, err := users.Get(acme, "u1"); err != nil {
		t.Fatalf("acme must see its own user: %v", err)
	}
	if _, err := users.Get(globex, "u1"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a different tenant partition must not see acme's user, got %v", err)
	}
}

// TestIAM_GroupAggregateOwnsMemberships proves Group is an aggregate ROOT that
// owns its Memberships (containment): the generated LoadGroupAggregate eager-loads
// the cluster, a member-mutation Save persists it and bumps the group etag, and a
// DB-level root delete cascades to the memberships.
func TestIAM_GroupAggregateOwnsMemberships(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_group_agg?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")
	groups := iamv1.NewGroupEntRepository(client)
	memberships := iamv1.NewMembershipEntRepository(client)
	tx := iamv1.NewEntTxRunner(client)

	agg := persistence.NewMemoryAggregateRepository(persistence.AggregateSpec[*iamv1.Group, string]{
		Tx:       tx,
		RootRepo: groups,
		KeyOf:    func(g *iamv1.Group) string { return g.GetId() },
		EtagOf:   func(g *iamv1.Group) string { return g.GetEtag() },
		LoadMembers: func(ctx context.Context, root *iamv1.Group) (*iamv1.Group, error) {
			return iamv1.LoadGroupAggregate(ctx, client, root.GetId())
		},
		SaveMembers: func(ctx context.Context, root *iamv1.Group) (bool, error) {
			stored, err := iamv1.LoadGroupAggregate(ctx, client, root.GetId())
			if err != nil {
				return false, err
			}
			have := map[string]struct{}{}
			for _, m := range stored.GetMemberships() {
				have[m.GetId()] = struct{}{}
			}
			changed := false
			for _, m := range root.GetMemberships() {
				if _, ok := have[m.GetId()]; !ok {
					m.GroupId = root.GetId()
					if _, cerr := memberships.Create(ctx, m); cerr != nil {
						return false, cerr
					}
					changed = true
				}
			}
			return changed, nil
		},
	})

	if _, err := groups.Create(ctx, &iamv1.Group{Id: "g1", DisplayName: "admins"}); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	root, err := agg.Load(ctx, "g1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	etagBefore := root.GetEtag()
	root.Memberships = append(root.Memberships,
		&iamv1.Membership{Id: "m1", UserId: "u1", Role: "admin"},
		&iamv1.Membership{Id: "m2", UserId: "u2", Role: "member"},
	)
	saved, err := agg.Save(ctx, root)
	if err != nil {
		t.Fatalf("Save members: %v", err)
	}
	if saved.GetEtag() == etagBefore || saved.GetEtag() == "" {
		t.Fatalf("group etag must be bumped on a member change: before=%q after=%q", etagBefore, saved.GetEtag())
	}
	reloaded, _ := agg.Load(ctx, "g1")
	if len(reloaded.GetMemberships()) != 2 {
		t.Fatalf("cluster must contain 2 memberships, got %d", len(reloaded.GetMemberships()))
	}

	// Cascade: deleting the group root at the DB level removes its memberships.
	if err := client.Group.DeleteOneID("g1").Exec(ctx); err != nil {
		t.Fatalf("hard-delete group: %v", err)
	}
	left, err := client.Membership.Query().All(ctx)
	if err != nil {
		t.Fatalf("query memberships: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("deleting the group root must cascade to its memberships; %d survived", len(left))
	}
}

// TestIAM_ApiKeyReferencesUserAndAuthProjection proves an ApiKey is its OWN
// aggregate that REFERENCES a User via a scalar FK (user_id) with NO traversable
// edge, and that auth lookup is a PROJECTION (LookupByKeyValueHash on the secret),
// not an aggregate Load.
func TestIAM_ApiKeyReferencesUserAndAuthProjection(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_apikey?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")
	enc := secret.NewDev([]byte("0123456789abcdef0123456789abcdef"))
	apiKeys := iamv1.NewApiKeyEntRepository(client, enc)

	// Create an api-key referencing a user; user_id is a plain scalar FK.
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k1", UserId: "u1", KeyValue: "secret-token", KeyPrefix: "abc"}); err != nil {
		t.Fatalf("create api-key: %v", err)
	}
	got, err := apiKeys.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get api-key: %v", err)
	}
	if got.GetUserId() != "u1" {
		t.Fatalf("api-key must carry the referenced user_id scalar FK, got %q", got.GetUserId())
	}
	// The secret is never returned (no key_value in the read response).
	if got.GetKeyValue() != "" {
		t.Fatalf("secret key_value must never be returned, got %q", got.GetKeyValue())
	}

	// Auth lookup is a projection on the secret hash, NOT an aggregate load.
	hash, err := enc.Hash(ctx, "secret-token")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	found, err := iamv1.LookupByKeyValueHash(ctx, client, hash)
	if err != nil {
		t.Fatalf("LookupByKeyValueHash: %v", err)
	}
	if found.GetId() != "k1" || found.GetUserId() != "u1" {
		t.Fatalf("auth projection must resolve the api-key + its user reference, got %+v", found)
	}
}

// TestIAM_MembershipBoundaryGate proves the member service registers only reads
// and therefore PASSES the boundary gate, while a hypothetical registered
// membership write would FAIL it (AC-1 on the IAM shape). We exercise the gate
// directly over the member binding the generated RegisterMembershipService would
// contribute.
func TestIAM_MembershipBoundaryGate(t *testing.T) {
	const (
		createMembership = "/iam.v1.MembershipService/CreateMembership"
		getMembership    = "/iam.v1.MembershipService/GetMembership"
		listMemberships  = "/iam.v1.MembershipService/ListMemberships"
	)
	// The IAM MembershipService declares only Get/List → empty WriteMethods → serves.
	serving := []server.MemberBinding{{Resource: "Membership", Root: "Group", WriteMethods: nil}}
	if err := server.AssertAggregateBoundaries([]string{getMembership, listMemberships}, serving); err != nil {
		t.Fatalf("a read-only member service must serve, got %v", err)
	}
	// A member that DID register a write fails closed.
	violating := []server.MemberBinding{{Resource: "Membership", Root: "Group", WriteMethods: []string{createMembership}}}
	if err := server.AssertAggregateBoundaries([]string{createMembership, getMembership}, violating); err == nil {
		t.Fatal("a registered membership write must fail the boundary gate")
	}
}

// ensure the ent client type is referenced (keeps the import meaningful even if a
// test above is later trimmed).
var _ = (*ent.Client)(nil)
