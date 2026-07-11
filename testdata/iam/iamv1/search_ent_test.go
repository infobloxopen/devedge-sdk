package iamv1_test

// search_ent_test.go — WS-041 full-text search integration test on the ent
// backend (FR-B4, AC-1/AC-2/AC-3). It exercises the generated `q` predicate that
// protoc-gen-ent emits in the User repository's List_ closure — a raw sql.P that
// branches on the runtime dialect (b.Dialect()) and binds the user term as an arg:
//
//   - SQLite (the fast dev/test driver, no to_tsvector): the portable
//     case-insensitive LIKE contains over email + display_name. Proven end to end
//     over the REAL generated gRPC service (ListUsers?q=...) AND at the repository
//     layer composing with an AIP-160 filter.
//   - Postgres (the production target): true FTS —
//     to_tsvector('simple', <email||display_name>) @@ websearch_to_tsquery('simple', $1)
//     — via a testcontainers postgres:16 harness (openIAMEntPG). The user term is a
//     bound parameter, so a hostile query is well-formed, not an injection (AC-3).
//
// User's search surface is portable (two field-flagged searchable string columns,
// no sql/postgres source), so the same generated code runs on both engines.

import (
	"context"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite" // register the SQLite driver for enttest

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/server"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/ent/enttest"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

// searchUsers are the fixture rows. `q=acme` must match alice + carol (each has
// the token "acme" in its email or display name) and never bob.
//
//	alice  alice@acme.test    "Alice Anderson"   -> matches q=acme (email)
//	bob    bob@globex.test    "Bob Brown"         -> does NOT match q=acme
//	carol  carol@other.test   "Carol of Acme"     -> matches q=acme (display_name)
var searchUsers = []*iamv1.User{
	{Id: "alice", Email: "alice@acme.test", DisplayName: "Alice Anderson"},
	{Id: "bob", Email: "bob@globex.test", DisplayName: "Bob Brown"},
	{Id: "carol", Email: "carol@other.test", DisplayName: "Carol of Acme"},
}

func seedSearchUsers(t *testing.T, ctx context.Context, repo persistence.Repository[*iamv1.User, string]) {
	t.Helper()
	for _, u := range searchUsers {
		clone := &iamv1.User{Id: u.Id, Email: u.Email, DisplayName: u.DisplayName}
		if _, err := repo.Create(ctx, clone); err != nil {
			t.Fatalf("seed user %s: %v", u.Id, err)
		}
	}
}

func userIDSet(users []*iamv1.User) []string {
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.GetId())
	}
	sort.Strings(ids)
	return ids
}

func eqUserIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestSearch_Ent_SQLite_QOverGeneratedService proves AC-1 on the ent+SQLite fast
// path: ListUsers?q=acme over the REAL generated gRPC UserService + generated ent
// repository returns only the rows whose searchable vector contains "acme", never
// the non-matching row. This flows request.q -> ListOptions.Search -> the
// generated ent sql.P predicate (SQLite LIKE branch).
func TestSearch_Ent_SQLite_QOverGeneratedService(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_search_svc?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := iamv1.NewUserEntRepository(client)

	seedSearchUsers(t, tenantCtx("tenant1"), repo)

	userClient := newUserSearchServer(t, repo)
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("account-id", "tenant1", "groups", "admin"))

	resp, err := userClient.ListUsers(ctx, &iamv1.ListUsersRequest{Q: "acme", PageSize: 100})
	if err != nil {
		t.Fatalf("ListUsers(q=acme): %v", err)
	}
	got := userIDSet(resp.Users)
	want := []string{"alice", "carol"}
	if !eqUserIDs(got, want) {
		t.Fatalf("q=acme over generated service: got %v, want %v (bob must not match)", got, want)
	}

	// An empty q is a no-op: all rows returned (SD-1/FR-B1).
	all, err := userClient.ListUsers(ctx, &iamv1.ListUsersRequest{PageSize: 100})
	if err != nil {
		t.Fatalf("ListUsers(no q): %v", err)
	}
	if len(all.Users) != len(searchUsers) {
		t.Fatalf("empty q should be a no-op returning all %d rows, got %d", len(searchUsers), len(all.Users))
	}
}

// TestSearch_Ent_SQLite_QComposesWithFilter proves AC-2 on the ent+SQLite path:
// the `q` predicate is ANDed with the AIP-160 filter in the generated ent List_,
// so q=acme & filter=display_name="Carol of Acme" returns only the row matching
// BOTH — no operator dropped (SD-6).
func TestSearch_Ent_SQLite_QComposesWithFilter(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_search_compose?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := iamv1.NewUserEntRepository(client)
	ctx := tenantCtx("tenant1")
	seedSearchUsers(t, ctx, repo)

	items, _, err := repo.List(ctx, persistence.ListOptions{
		Search: "acme",
		Filter: `display_name="Carol of Acme"`,
	})
	if err != nil {
		t.Fatalf("List(q=acme, filter=display_name): %v", err)
	}
	got := userIDSet(items)
	want := []string{"carol"} // acme (search) matches alice+carol; the filter narrows to carol
	if !eqUserIDs(got, want) {
		t.Fatalf("q composes with filter: got %v, want %v", got, want)
	}
}

// TestSearch_Ent_SQLite_WhitespaceQIsNoOp proves the WS-041 F5 fix on the ent
// backend: a whitespace-only q is a no-op identical to an empty q (FR-B1), not a
// real zero-matching query. The generated ent List_ now strings.TrimSpace()s the
// term and skips the predicate when nothing is left.
func TestSearch_Ent_SQLite_WhitespaceQIsNoOp(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:iam_search_ws?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	repo := iamv1.NewUserEntRepository(client)
	ctx := tenantCtx("tenant1")
	seedSearchUsers(t, ctx, repo)

	empty, _, err := repo.List(ctx, persistence.ListOptions{Search: ""})
	if err != nil {
		t.Fatalf("List(q=\"\"): %v", err)
	}
	if len(empty) != len(searchUsers) {
		t.Fatalf("empty q should return all %d rows, got %d", len(searchUsers), len(empty))
	}
	for _, ws := range []string{" ", "   ", "\t", " \t\n "} {
		got, _, err := repo.List(ctx, persistence.ListOptions{Search: ws})
		if err != nil {
			t.Fatalf("List(q=%q): %v", ws, err)
		}
		if !eqUserIDs(userIDSet(got), userIDSet(empty)) {
			t.Errorf("whitespace-only q=%q must be a no-op like empty q: got %v, want %v",
				ws, userIDSet(got), userIDSet(empty))
		}
	}
}

// TestSearch_Ent_Postgres_QIsTrueFTS proves the ent `q` predicate does REAL
// Postgres full-text search (FR-B4, AC-1 PG) over a testcontainers postgres:16 —
// the Postgres branch of the generated sql.P (to_tsvector @@ websearch_to_tsquery).
// It also proves injection safety (AC-3): a hostile query string is a bound
// parameter, so it yields a well-formed result, never a SQL error. Skips cleanly
// when Docker is unavailable (openIAMEntPG -> startPostgres -> t.Skip).
func TestSearch_Ent_Postgres_QIsTrueFTS(t *testing.T) {
	client := openIAMEntPG(t) // t.Skip's when Docker is unavailable
	repo := iamv1.NewUserEntRepository(client)
	ctx := tenantCtx("tenant1")
	seedSearchUsers(t, ctx, repo)

	items, _, err := repo.List(ctx, persistence.ListOptions{Search: "acme"})
	if err != nil {
		t.Fatalf("List(q=acme) on postgres: %v", err)
	}
	got := userIDSet(items)
	want := []string{"alice", "carol"}
	if !eqUserIDs(got, want) {
		t.Fatalf("postgres true FTS q=acme: got %v, want %v (bob must not match)", got, want)
	}

	// Composition on Postgres: q=acme AND filter narrows to carol (AC-2 on PG).
	composed, _, err := repo.List(ctx, persistence.ListOptions{
		Search: "acme",
		Filter: `display_name="Carol of Acme"`,
	})
	if err != nil {
		t.Fatalf("List(q=acme, filter) on postgres: %v", err)
	}
	if got := userIDSet(composed); !eqUserIDs(got, []string{"carol"}) {
		t.Fatalf("postgres q composes with filter: got %v, want [carol]", got)
	}

	// AC-3 injection safety: a hostile term is a bound arg to websearch_to_tsquery,
	// which accepts free-form text — the query is well-formed, not a SQL error.
	if _, _, err := repo.List(ctx, persistence.ListOptions{Search: `ac(me" or 1=1 --`}); err != nil {
		t.Fatalf("hostile q must be a safe bound parameter, got error: %v", err)
	}
}

// newUserSearchServer boots a real gRPC server backed by the ent User repository
// and returns a connected UserService client. It registers the GENERATED CRUD
// handler over the generated repository, so ListUsers flows request.q ->
// ListOptions.Search -> the generated ent search predicate.
func newUserSearchServer(t *testing.T, repo persistence.Repository[*iamv1.User, string]) iamv1.UserServiceClient {
	t.Helper()
	s, err := server.New(server.Config{
		GRPCAddr:      ":0",
		Authorizer:    authz.NewDevAuthorizer(authz.Grant{Tenant: "*", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*"}),
		PrincipalFunc: grpcauthz.DevPrincipalFunc(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := iamv1.RegisterUserServiceWithRepository(s, repo); err != nil {
		t.Fatalf("RegisterUserServiceWithRepository: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	var addr string
	for time.Now().Before(deadline) {
		if a := s.GRPCAddr(); a != "" && a != ":0" {
			addr = a
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("server did not bind gRPC address within 2s")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return iamv1.NewUserServiceClient(conn)
}
