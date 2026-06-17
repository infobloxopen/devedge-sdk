package seccheck

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// --- AssertUnknownPrincipalDenied tests ---

func TestAssertUnknownPrincipalDenied_AllDenied(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc/MethodA", Verb: "read", Resource: "widget"},
		{Method: "/svc/MethodB", Verb: "write", Resource: "widget"},
	}
	calls := map[string]CallFn{
		"/svc/MethodA": func(ctx context.Context) error {
			return status.Error(codes.PermissionDenied, "denied")
		},
		"/svc/MethodB": func(ctx context.Context) error {
			return status.Error(codes.PermissionDenied, "denied")
		},
	}
	findings := AssertUnknownPrincipalDenied(context.Background(), rules, calls)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
}

func TestAssertUnknownPrincipalDenied_OnePasses(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc/MethodA", Verb: "read", Resource: "widget"},
		{Method: "/svc/MethodB", Verb: "write", Resource: "widget"},
	}
	calls := map[string]CallFn{
		"/svc/MethodA": func(ctx context.Context) error {
			return nil // should have been denied
		},
		"/svc/MethodB": func(ctx context.Context) error {
			return status.Error(codes.PermissionDenied, "denied")
		},
	}
	findings := AssertUnknownPrincipalDenied(context.Background(), rules, calls)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != Error {
		t.Errorf("expected Error severity, got %v", findings[0].Severity)
	}
	if findings[0].Method != "/svc/MethodA" {
		t.Errorf("expected finding for /svc/MethodA, got %q", findings[0].Method)
	}
}

func TestAssertUnknownPrincipalDenied_PublicSkipped(t *testing.T) {
	called := false
	rules := []authz.MethodRule{
		{Method: "/svc/PublicMethod", Public: true},
	}
	calls := map[string]CallFn{
		"/svc/PublicMethod": func(ctx context.Context) error {
			called = true
			return nil
		},
	}
	findings := AssertUnknownPrincipalDenied(context.Background(), rules, calls)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
	if called {
		t.Error("CallFn for a Public method should never be invoked")
	}
}

// --- AssertErrorMessagesClean tests ---

func TestAssertErrorMessagesClean_Clean(t *testing.T) {
	triggers := []ErrorTrigger{
		{
			Method: "/svc/Method",
			Fn: func(ctx context.Context) error {
				return status.Error(codes.NotFound, "not found")
			},
		},
	}
	findings := AssertErrorMessagesClean(context.Background(), triggers)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
}

func TestAssertErrorMessagesClean_LeaksPersistencePrefix(t *testing.T) {
	triggers := []ErrorTrigger{
		{
			Method: "/svc/Method",
			Fn: func(ctx context.Context) error {
				return status.Error(codes.NotFound, "persistence: not found")
			},
		},
	}
	findings := AssertErrorMessagesClean(context.Background(), triggers)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != Error {
		t.Errorf("expected Error severity, got %v", findings[0].Severity)
	}
}

func TestAssertErrorMessagesClean_LeaksSQLKeyword(t *testing.T) {
	triggers := []ErrorTrigger{
		{
			Method: "/svc/Method",
			Fn: func(ctx context.Context) error {
				return status.Error(codes.Internal, "WHERE id = 'foo'")
			},
		},
	}
	findings := AssertErrorMessagesClean(context.Background(), triggers)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != Error {
		t.Errorf("expected Error severity, got %v", findings[0].Severity)
	}
}

// Issue 017: a raw DB constraint violation carries none of the SQL keywords
// (SELECT/INSERT/WHERE/…) but still leaks the table/column schema. The gate must
// flag it.
func TestAssertErrorMessagesClean_LeaksDBConstraint(t *testing.T) {
	leaks := []string{
		"UNIQUE constraint failed: destination_models.name",          // SQLite
		`pq: duplicate key value violates unique constraint "ux_dst"`, // PostgreSQL
		"create Destination: ERROR ... (SQLSTATE 23505)",              // pg via gorm
	}
	for _, msg := range leaks {
		msg := msg
		t.Run(msg, func(t *testing.T) {
			findings := AssertErrorMessagesClean(context.Background(), []ErrorTrigger{{
				Method: "/svc/Create",
				Fn:     func(ctx context.Context) error { return status.Error(codes.Internal, msg) },
			}})
			if len(findings) == 0 || findings[0].Severity != Error {
				t.Errorf("expected an Error finding for leaking message %q, got %+v", msg, findings)
			}
		})
	}
}

func TestAssertErrorMessagesClean_UnexpectedSuccess(t *testing.T) {
	triggers := []ErrorTrigger{
		{
			Method: "/svc/Method",
			Fn: func(ctx context.Context) error {
				return nil // expected an error but got none
			},
		},
	}
	findings := AssertErrorMessagesClean(context.Background(), triggers)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Severity != Warning {
		t.Errorf("expected Warning severity, got %v", findings[0].Severity)
	}
}

// --- AssertCrossAccountIsolation soft-delete tests (F020 / FR-015) ---

// fakeSoftDeleteStore is a trivial in-memory store for isolation testing.
type fakeSoftDeleteStore struct {
	items   map[string]bool // id → exists
	deleted map[string]bool // id → soft-deleted
}

func newFakeStore() *fakeSoftDeleteStore {
	return &fakeSoftDeleteStore{items: map[string]bool{}, deleted: map[string]bool{}}
}

func (s *fakeSoftDeleteStore) create(accountID string) (string, error) {
	id := accountID + "-item"
	s.items[id] = true
	return id, nil
}

func (s *fakeSoftDeleteStore) read(accountID, id string) error {
	if !s.items[id] || s.deleted[id] {
		return status.Error(codes.NotFound, "not found")
	}
	// Tenant check: only the owning account can read.
	if id != accountID+"-item" {
		return status.Error(codes.NotFound, "not found")
	}
	return nil
}

func (s *fakeSoftDeleteStore) softDelete(accountID, id string) error {
	if !s.items[id] {
		return status.Error(codes.NotFound, "not found")
	}
	s.deleted[id] = true
	return nil
}

func (s *fakeSoftDeleteStore) listDeleted(accountID string) (int, error) {
	count := 0
	for id, del := range s.deleted {
		if del && id == accountID+"-item" {
			count++
		}
	}
	return count, nil
}

func TestAssertCrossAccountIsolation_SoftDeleteIsolated(t *testing.T) {
	store := newFakeStore()
	cfg := IsolationConfig{
		PrincipalA: "tenantA",
		PrincipalB: "tenantB",
		CreateFn: func(ctx context.Context) (string, error) {
			return store.create("tenantA")
		},
		ReadFn: func(ctx context.Context, id string) error {
			return store.read("tenantB", id)
		},
		DeleteFn: func(ctx context.Context, id string) error {
			return store.softDelete("tenantA", id)
		},
		ListDeletedFn: func(ctx context.Context) (int, error) {
			return store.listDeleted("tenantB")
		},
	}
	findings := AssertCrossAccountIsolation(context.Background(), cfg)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (properly isolated), got %d: %+v", len(findings), findings)
	}
}

func TestAssertCrossAccountIsolation_SoftDeleteLeaks(t *testing.T) {
	// A store that leaks soft-deleted rows across tenants.
	leaked := false
	cfg := IsolationConfig{
		PrincipalA: "tenantA",
		PrincipalB: "tenantB",
		CreateFn: func(ctx context.Context) (string, error) {
			return "shared-id", nil
		},
		ReadFn: func(ctx context.Context, id string) error {
			// B can't read live — returns NotFound.
			return status.Error(codes.NotFound, "not found")
		},
		DeleteFn: func(ctx context.Context, id string) error {
			return nil // soft-delete succeeds
		},
		ListDeletedFn: func(ctx context.Context) (int, error) {
			// Bug: B can see A's soft-deleted item.
			leaked = true
			return 1, nil
		},
	}
	findings := AssertCrossAccountIsolation(context.Background(), cfg)
	if !leaked {
		t.Fatal("ListDeletedFn was never called")
	}
	found := false
	for _, f := range findings {
		if f.Method == "(list-deleted)" && f.Severity == Error {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a list-deleted Error finding, got %+v", findings)
	}
}

func TestAssertCrossAccountIsolation_SoftDeleteNilFns_NoChange(t *testing.T) {
	// When DeleteFn/ListDeletedFn are nil, behavior is unchanged from before.
	cfg := IsolationConfig{
		PrincipalA: "tenantA",
		PrincipalB: "tenantB",
		CreateFn: func(ctx context.Context) (string, error) {
			return "item-1", nil
		},
		ReadFn: func(ctx context.Context, id string) error {
			return status.Error(codes.NotFound, "not found")
		},
	}
	findings := AssertCrossAccountIsolation(context.Background(), cfg)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
}
