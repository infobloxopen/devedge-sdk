package widgetsv1_test

// search_sqlite_test.go — WS-041 full-text search integration test on the GORM
// SQLite fast path (FR-B5, AC-1/AC-2). It exercises the generated `q` predicate
// end-to-end: the `q` operator matches only rows whose searchable vector contains
// the term, and composes (AND) with an AIP-160 filter. The toy Widget's search
// surface is portable (a field-flagged display_name plus a calculated source with
// a `cel` alternate), so it degrades to a case-insensitive LIKE contains on
// SQLite — no Postgres container needed for this fast test.

import (
	"context"
	"sort"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/server"
	"github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1"
)

// openWidgetDB opens an in-memory SQLite GORM database with the WidgetModel
// schema migrated.
func openWidgetDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:widgets_search?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open search test db: %v", err)
	}
	if err := db.AutoMigrate(&widgetsv1.WidgetModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// seedSearchWidgets creates the fixture rows and returns the repository.
//
//	w-alpha  "acme alpha"  category=standard  -> matches q=acme
//	w-beta   "globex beta" category=premium   -> does NOT match q=acme
//	w-gamma  "acme gamma"  category=premium   -> matches q=acme
func seedSearchWidgets(t *testing.T, repo *widgetsv1.WidgetRepository) {
	t.Helper()
	ctx := context.Background()
	for _, w := range []*widgetsv1.Widget{
		{Id: "w-alpha", DisplayName: "acme alpha", Category: "standard"},
		{Id: "w-beta", DisplayName: "globex beta", Category: "premium"},
		{Id: "w-gamma", DisplayName: "acme gamma", Category: "premium"},
	} {
		if _, err := repo.Create(ctx, w); err != nil {
			t.Fatalf("Create %s: %v", w.Id, err)
		}
	}
}

func idSet(widgets []*widgetsv1.Widget) []string {
	ids := make([]string, 0, len(widgets))
	for _, w := range widgets {
		ids = append(ids, w.Id)
	}
	sort.Strings(ids)
	return ids
}

func eqIDs(got, want []string) bool {
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

// TestSearch_SQLite_QOverGeneratedService proves AC-1 on the SQLite fast path:
// GET-equivalent ListWidgets?q=acme over the REAL generated gRPC service +
// generated GORM repository returns only the rows whose searchable vector
// contains "acme", and never the non-matching row.
func TestSearch_SQLite_QOverGeneratedService(t *testing.T) {
	db := openWidgetDB(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := widgetsv1.NewWidgetRepository(db, secret.NewDev(make([]byte, 32)))
	seedSearchWidgets(t, repo)

	grpcAddr := newGORMWidgetServer(t, repo)
	conn := dialGRPC(t, grpcAddr)
	client := widgetsv1.NewWidgetServiceClient(conn)
	ctx := ctxWithMD("account-id", "t1")

	resp, err := client.ListWidgets(ctx, &widgetsv1.ListWidgetsRequest{Q: "acme", PageSize: 100})
	if err != nil {
		t.Fatalf("ListWidgets(q=acme): %v", err)
	}
	got := idSet(resp.Widgets)
	want := []string{"w-alpha", "w-gamma"}
	if !eqIDs(got, want) {
		t.Fatalf("q=acme over generated service: got %v, want %v (w-beta must not match)", got, want)
	}

	// An empty q is a no-op: all rows returned (SD-1/FR-B1).
	all, err := client.ListWidgets(ctx, &widgetsv1.ListWidgetsRequest{PageSize: 100})
	if err != nil {
		t.Fatalf("ListWidgets(no q): %v", err)
	}
	if len(all.Widgets) != 3 {
		t.Fatalf("empty q should be a no-op returning all 3 rows, got %d", len(all.Widgets))
	}
}

// TestSearch_SQLite_WhitespaceQIsNoOp proves the WS-041 F5 fix: a whitespace-only q
// is a no-op identical to an empty q (FR-B1), NOT a real zero-matching query. Before
// the fix the generated List guarded on `opts.Search != ""`, so q="   " ran the FTS
// predicate against a blank term and returned nothing while an empty q returned all
// rows. The generated List now strings.TrimSpace()s the term and skips the predicate
// when nothing is left, so both return every row.
func TestSearch_SQLite_WhitespaceQIsNoOp(t *testing.T) {
	db := openWidgetDB(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := widgetsv1.NewWidgetRepository(db, secret.NewDev(make([]byte, 32)))
	seedSearchWidgets(t, repo)
	ctx := context.Background()

	// The baseline: an empty q returns all rows.
	empty, _, err := repo.List(ctx, persistence.ListOptions{Search: "", PageSize: 100})
	if err != nil {
		t.Fatalf("List(q=\"\"): %v", err)
	}
	if len(empty) != 3 {
		t.Fatalf("empty q should return all 3 rows, got %d", len(empty))
	}

	// A whitespace-only q must behave identically to the empty q — all rows, not the
	// zero rows a blank FTS/LIKE term would match.
	for _, ws := range []string{" ", "   ", "\t", " \t\n "} {
		got, _, err := repo.List(ctx, persistence.ListOptions{Search: ws, PageSize: 100})
		if err != nil {
			t.Fatalf("List(q=%q): %v", ws, err)
		}
		if !eqIDs(idSet(got), idSet(empty)) {
			t.Errorf("whitespace-only q=%q must be a no-op like empty q: got %v, want %v",
				ws, idSet(got), idSet(empty))
		}
	}
}

// TestSearch_SQLite_QComposesWithFilter proves AC-2 on the SQLite fast path: the
// `q` predicate is ANDed with the AIP-160 filter in the generated GORM List, so
// q=acme & filter=category="premium" returns only the row matching BOTH — no
// operator dropped (SD-6).
func TestSearch_SQLite_QComposesWithFilter(t *testing.T) {
	db := openWidgetDB(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := widgetsv1.NewWidgetRepository(db, secret.NewDev(make([]byte, 32)))
	seedSearchWidgets(t, repo)
	ctx := context.Background()

	items, _, err := repo.List(ctx, persistence.ListOptions{
		Search: "acme",
		Filter: `category="premium"`,
	})
	if err != nil {
		t.Fatalf("List(q=acme, filter=category=premium): %v", err)
	}
	got := idSet(items)
	want := []string{"w-gamma"} // acme (search) AND premium (filter): only w-gamma
	if !eqIDs(got, want) {
		t.Fatalf("q composes with filter: got %v, want %v", got, want)
	}
}

// TestSearch_SQLite_LikeMetacharsAreLiteral proves SEC-041-01: the SQLite LIKE
// fallback treats a search term's LIKE metacharacters (%, _, \) as LITERAL text,
// not wildcards. A term of "%" must match ONLY rows whose searchable text contains
// a literal '%', never every row; a term of "_" only rows containing '_'. Before
// the fix the term was bound un-escaped, so q="%" matched everything (a wildcard).
func TestSearch_SQLite_LikeMetacharsAreLiteral(t *testing.T) {
	db := openWidgetDB(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := widgetsv1.NewWidgetRepository(db, secret.NewDev(make([]byte, 32)))
	ctx := context.Background()
	for _, w := range []*widgetsv1.Widget{
		{Id: "w-pct", DisplayName: "50% off sale", Category: "standard"},   // literal %
		{Id: "w-under", DisplayName: "sale_price drop", Category: "standard"}, // literal _
		{Id: "w-plain", DisplayName: "regular price", Category: "standard"},   // neither
	} {
		if _, err := repo.Create(ctx, w); err != nil {
			t.Fatalf("Create %s: %v", w.Id, err)
		}
	}

	cases := []struct {
		q    string
		want []string
	}{
		{"%", []string{"w-pct"}},     // NOT a wildcard-all: only the literal-% row
		{"50%", []string{"w-pct"}},   // a term containing % matches the literal-% row
		{"_", []string{"w-under"}},   // '_' is literal too: only the literal-_ row
		{"nomatch", []string{}},      // an ordinary miss still misses
	}
	for _, tc := range cases {
		items, _, err := repo.List(ctx, persistence.ListOptions{Search: tc.q, PageSize: 100})
		if err != nil {
			t.Fatalf("List(q=%q): %v", tc.q, err)
		}
		if got := idSet(items); !eqIDs(got, tc.want) {
			t.Errorf("q=%q: got %v, want %v (metacharacters must be literal, not wildcards)", tc.q, got, tc.want)
		}
	}
}

// newGORMWidgetServer boots a real gRPC server backed by the GORM repository and
// returns its bound address. It registers the GENERATED CRUD handler over the
// generated repository, so ListWidgets flows request.q -> ListOptions.Search ->
// the generated GORM search predicate.
func newGORMWidgetServer(t *testing.T, repo *widgetsv1.WidgetRepository) string {
	t.Helper()
	permissive := authz.NewDevAuthorizer(authz.Grant{
		Tenant:   "*",
		Subjects: []string{"*"},
		Verbs:    []authz.Verb{"*"},
		Resource: "*",
	})
	s, err := server.New(server.Config{
		GRPCAddr:   ":0",
		Authorizer: permissive,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := widgetsv1.RegisterWidgetServiceWithRepository(s, repo); err != nil {
		t.Fatalf("RegisterWidgetServiceWithRepository: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.GRPCAddr(); addr != "" && addr != ":0" {
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server did not bind gRPC address within 2s")
	return ""
}
