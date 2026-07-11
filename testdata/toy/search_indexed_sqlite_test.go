package widgetsv1_test

// search_indexed_sqlite_test.go — WS-041 INDEXED-strategy fixture on the GORM
// SQLite fast path (FR-C3). INDEXED materializes the search vector as a persisted
// `search_vector` generated column + GIN index ON POSTGRES (proven in
// search_indexed_pg_test.go). SQLite has no tsvector and no persisted column, so an
// INDEXED resource keeps the same query-time LIKE fallback as JIT (FR-C3): the
// generated GizmoRepository.List branches on the runtime dialect, and on SQLite the
// portable `label` + cel-alternate `tier_label` vector drives a case-insensitive
// LIKE contains. This proves the INDEXED resource is exercised end to end without a
// Postgres container, and that its SQLite path is unaffected by the strategy.

import (
	"context"
	"sort"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1"
)

func openGizmoDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:gizmos_search?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open gizmo test db: %v", err)
	}
	if err := db.AutoMigrate(&widgetsv1.GizmoModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// seedGizmos creates the fixture rows and returns the repository.
//
//	g-alpha  "acme alpha"  category=standard  -> label matches q=acme; tier "standard basic"
//	g-beta   "globex beta" category=premium   -> label no acme; tier "premium ..." matches q=premium
//	g-gamma  "acme gamma"  category=premium   -> label matches q=acme; tier premium matches q=premium
func seedGizmos(t *testing.T, repo *widgetsv1.GizmoRepository) {
	t.Helper()
	ctx := context.Background()
	for _, g := range []*widgetsv1.Gizmo{
		{Id: "g-alpha", Label: "acme alpha", Category: "standard"},
		{Id: "g-beta", Label: "globex beta", Category: "premium"},
		{Id: "g-gamma", Label: "acme gamma", Category: "premium"},
	} {
		if _, err := repo.Create(ctx, g); err != nil {
			t.Fatalf("Create %s: %v", g.Id, err)
		}
	}
}

func gizmoIDs(gs []*widgetsv1.Gizmo) []string {
	ids := make([]string, 0, len(gs))
	for _, g := range gs {
		ids = append(ids, g.Id)
	}
	sort.Strings(ids)
	return ids
}

// TestIndexedSearch_SQLite_FieldSource proves the INDEXED resource's field-flagged
// `label` source matches over the SQLite LIKE fallback (FR-C3): q=acme returns only
// the rows whose label contains "acme".
func TestIndexedSearch_SQLite_FieldSource(t *testing.T) {
	db := openGizmoDB(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := widgetsv1.NewGizmoRepository(db)
	seedGizmos(t, repo)
	ctx := context.Background()

	items, _, err := repo.List(ctx, persistence.ListOptions{Search: "acme"})
	if err != nil {
		t.Fatalf("List(q=acme): %v", err)
	}
	got := gizmoIDs(items)
	want := []string{"g-alpha", "g-gamma"}
	if !eqIDs(got, want) {
		t.Fatalf("INDEXED q=acme on SQLite: got %v, want %v", got, want)
	}

	// Empty q is a no-op: all rows.
	all, _, err := repo.List(ctx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("List(no q): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("empty q should return all 3 gizmos, got %d", len(all))
	}
}

// TestIndexedSearch_SQLite_CELCalculatedSource proves the message-level calculated
// source (`tier_label`) contributes to the SQLite vector via its portable `cel`
// alternate: q=premium matches the rows the CASE maps to a "premium" tier, i.e. the
// premium-category rows (FR-C3, SD-9). The sql/postgres CASE alternate is exercised
// on Postgres (AC-8); on SQLite the cel form keeps the resource portable.
func TestIndexedSearch_SQLite_CELCalculatedSource(t *testing.T) {
	db := openGizmoDB(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := widgetsv1.NewGizmoRepository(db)
	seedGizmos(t, repo)
	ctx := context.Background()

	items, _, err := repo.List(ctx, persistence.ListOptions{Search: "premium"})
	if err != nil {
		t.Fatalf("List(q=premium): %v", err)
	}
	got := gizmoIDs(items)
	want := []string{"g-beta", "g-gamma"} // both premium-category rows
	if !eqIDs(got, want) {
		t.Fatalf("INDEXED q=premium (cel tier source) on SQLite: got %v, want %v", got, want)
	}
}
