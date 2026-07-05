package iam_test

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/filter"
	"github.com/infobloxopen/devedge-sdk/servicekit"
	iamv1 "github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

// fakeApiKeyRepo is a minimal persistence.Repository that HONORS the AIP-160
// equality filter the generated nested List pushes down (unlike MemoryRepository,
// which ignores filters). It lets the behavioral test observe the #191 parent
// scoping end to end against the real generated handler.
type fakeApiKeyRepo struct {
	items []*iamv1.ApiKey
}

func newFakeApiKeyRepo(items ...*iamv1.ApiKey) *fakeApiKeyRepo { return &fakeApiKeyRepo{items: items} }

func (f *fakeApiKeyRepo) Get(_ context.Context, key string) (*iamv1.ApiKey, error) {
	for _, k := range f.items {
		if k.GetId() == key {
			return k, nil
		}
	}
	return nil, persistence.ErrNotFound
}

func (f *fakeApiKeyRepo) List(_ context.Context, opts persistence.ListOptions) ([]*iamv1.ApiKey, string, error) {
	want := ""
	if opts.Filter != "" {
		cond, err := filter.Parse(opts.Filter, map[string]string{"user_id": "user_id"})
		if err != nil {
			return nil, "", err
		}
		cmp, ok := cond.(*filter.Comparison)
		if !ok || cmp.Column != "user_id" || cmp.Op != "=" {
			return nil, "", fmt.Errorf("unexpected list filter %q", opts.Filter)
		}
		want = fmt.Sprint(cmp.Value)
	}
	var out []*iamv1.ApiKey
	for _, k := range f.items {
		if want == "" || k.GetUserId() == want {
			out = append(out, k)
		}
	}
	return out, "", nil
}

func (f *fakeApiKeyRepo) Create(context.Context, *iamv1.ApiKey) (*iamv1.ApiKey, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeApiKeyRepo) Update(context.Context, string, *iamv1.ApiKey, ...string) (*iamv1.ApiKey, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeApiKeyRepo) Delete(context.Context, string) error { return fmt.Errorf("not implemented") }
func (f *fakeApiKeyRepo) Undelete(context.Context, string) (*iamv1.ApiKey, error) {
	return nil, fmt.Errorf("not implemented")
}

// TestApiKeyService_NestedParentEnforced is the #191 regression: ApiKey is nested
// under its owning User (users/{user_id}/apikeys/{id}). The generated Get/List MUST
// enforce the URL parent — List returns only the parent's rows, and a cross-parent
// Get is denied — rather than binding {user_id} and ignoring it.
func TestApiKeyService_NestedParentEnforced(t *testing.T) {
	repo := newFakeApiKeyRepo(
		&iamv1.ApiKey{Id: "k1", UserId: "u1"},
		&iamv1.ApiKey{Id: "k2", UserId: "u2"},
		&iamv1.ApiKey{Id: "k3", UserId: "u2"},
	)
	h := iamv1.NewApiKeyServiceHandler(repo)
	ctx := context.Background()

	// List under u2 returns ONLY u2's keys (not u1's k1).
	resp, err := h.ListApiKeys(ctx, &iamv1.ListApiKeysRequest{UserId: "u2"})
	if err != nil {
		t.Fatalf("ListApiKeys(u2): %v", err)
	}
	if len(resp.GetApiKeys()) != 2 {
		t.Fatalf("ListApiKeys(u2) returned %d keys, want 2 (u2-scoped)", len(resp.GetApiKeys()))
	}
	for _, k := range resp.GetApiKeys() {
		if k.GetUserId() != "u2" {
			t.Errorf("ListApiKeys(u2) leaked a key owned by %q", k.GetUserId())
		}
	}

	// Cross-parent Get is DENIED: k1 belongs to u1, requested under u2.
	if _, err := h.GetApiKey(ctx, &iamv1.GetApiKeyRequest{Id: "k1", UserId: "u2"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetApiKey(k1 under u2) = %v, want NotFound (cross-parent denied)", err)
	}

	// Same-parent Get succeeds.
	got, err := h.GetApiKey(ctx, &iamv1.GetApiKeyRequest{Id: "k1", UserId: "u1"})
	if err != nil {
		t.Fatalf("GetApiKey(k1 under u1): %v", err)
	}
	if got.GetId() != "k1" {
		t.Errorf("GetApiKey(k1 under u1) = %q, want k1", got.GetId())
	}
}

// TestServiceModuleIDs_DistinctPerService is the #190 regression: all five iam
// services share the proto package iam.v1, so their default module ID is the same
// ("iam") and servicekit.Run rejects them as duplicates. The ID override lets a
// host give each a distinct ID and compose them together.
func TestServiceModuleIDs_DistinctPerService(t *testing.T) {
	// Default IDs (no override): two services from one proto collide.
	dflt := []servicekit.Descriptor{
		iamv1.AccountServiceModule(iamv1.AccountServiceModuleOptions{}).Descriptor(),
		iamv1.UserServiceModule(iamv1.UserServiceModuleOptions{}).Descriptor(),
	}
	if dflt[0].ID != "iam" || dflt[1].ID != "iam" {
		t.Fatalf("default module IDs = %q/%q, want both %q", dflt[0].ID, dflt[1].ID, "iam")
	}
	if err := servicekit.ValidateDescriptors(dflt); err == nil {
		t.Fatal("two services from one proto with default IDs must collide (the #190 bug); ValidateDescriptors returned nil")
	}

	// Distinct IDs via the override: all five host together.
	mods := []servicekit.Module{
		iamv1.AccountServiceModule(iamv1.AccountServiceModuleOptions{ID: "iam-account"}),
		iamv1.UserServiceModule(iamv1.UserServiceModuleOptions{ID: "iam-user"}),
		iamv1.GroupServiceModule(iamv1.GroupServiceModuleOptions{ID: "iam-group"}),
		iamv1.MembershipServiceModule(iamv1.MembershipServiceModuleOptions{ID: "iam-membership"}),
		iamv1.ApiKeyServiceModule(iamv1.ApiKeyServiceModuleOptions{ID: "iam-apikey"}),
	}
	descs := make([]servicekit.Descriptor, len(mods))
	for i, m := range mods {
		descs[i] = m.Descriptor()
	}
	if err := servicekit.ValidateDescriptors(descs); err != nil {
		t.Fatalf("distinct-ID modules should validate together, got: %v", err)
	}
	// The resource name is qualified by the EFFECTIVE (overridden) id.
	if got := descs[0].Resources[0].Name; got != "iam-account.account" {
		t.Errorf("Account resource name = %q, want %q", got, "iam-account.account")
	}
}
