package gormtx_test

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/cells"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// widgetRow is a tenant-scoped domain model (has account_id) used to exercise the
// write-guard end to end.
type widgetRow struct {
	ID        string `gorm:"primaryKey"`
	AccountID string `gorm:"column:account_id"`
	Name      string `gorm:"column:name"`
}

func (widgetRow) TableName() string { return "widgets" }

// gizmoRow is a NON-tenant-scoped domain model (no account_id) — the guard must skip
// it (no fence applies).
type gizmoRow struct {
	ID   string `gorm:"primaryKey"`
	Name string `gorm:"column:name"`
}

func (gizmoRow) TableName() string { return "gizmos" }

// openFenceDB opens a shared-cache in-memory SQLite db with the cell-based-development
// framework tables + the two domain models migrated.
func openFenceDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&gormtx.TenantFenceRow{}, &gormtx.TenantEventSeqRow{}, &gormtx.TenantEventPolicyRow{}, &widgetRow{}, &gizmoRow{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// TestGormFencer_ForwardOnly proves Seal/SetOwner reject a backward epoch and are
// idempotent on the same epoch.
func TestGormFencer_ForwardOnly(t *testing.T) {
	db := openFenceDB(t, "fence_forward")
	f := gormtx.NewGormFencer(db)
	ctx := context.Background()

	if err := f.SetOwner(ctx, "t1", "cell-a", 5); err != nil {
		t.Fatalf("SetOwner@5: %v", err)
	}
	if err := f.SetOwner(ctx, "t1", "cell-b", 6); err != nil {
		t.Fatalf("SetOwner@6 forward: %v", err)
	}
	if err := f.SetOwner(ctx, "t1", "cell-b", 6); err != nil {
		t.Fatalf("SetOwner@6 idempotent: %v", err)
	}
	if err := f.SetOwner(ctx, "t1", "cell-a", 5); !errors.Is(err, cells.ErrFenceRegression) {
		t.Fatalf("SetOwner backward must be ErrFenceRegression, got %v", err)
	}
	if err := f.Seal(ctx, "t1", 7); err != nil {
		t.Fatalf("Seal@7: %v", err)
	}
	if err := f.Seal(ctx, "t1", 6); !errors.Is(err, cells.ErrFenceRegression) {
		t.Fatalf("Seal backward must be ErrFenceRegression, got %v", err)
	}
}

// TestWriteGuard_AllowPredicate proves the fence predicate over a real fence table:
// no-row→allow, match→allow, wrong cell→reject, wrong epoch→reject, sealed→reject.
func TestWriteGuard_AllowPredicate(t *testing.T) {
	db := openFenceDB(t, "fence_allow")
	f := gormtx.NewGormFencer(db)
	ctx := context.Background()
	const tbl = "tenant_fence"

	mustAllow := func(name, tenant, cell string, epoch uint64, want bool) {
		ok, err := gormtx.WriteGuardAllowForTest(db, ctx, tbl, tenant, cells.AdmissionToken{TenantID: tenant, CellID: cell, RouteEpoch: epoch})
		if err != nil {
			t.Fatalf("%s: allow err: %v", name, err)
		}
		if ok != want {
			t.Fatalf("%s: allow=%v want %v", name, ok, want)
		}
	}

	mustAllow("no-fence-row", "t-unfenced", "cell-a", 1, true)

	if err := f.SetOwner(ctx, "t-owned", "cell-a", 3); err != nil {
		t.Fatal(err)
	}
	mustAllow("owner+epoch-match", "t-owned", "cell-a", 3, true)
	mustAllow("wrong-cell", "t-owned", "cell-b", 3, false)
	mustAllow("wrong-epoch", "t-owned", "cell-a", 2, false)

	if err := f.Seal(ctx, "t-owned", 4); err != nil {
		t.Fatal(err)
	}
	mustAllow("sealed-rejects-owner", "t-owned", "cell-a", 3, false)
}

// TestWriteGuard_FrameworkTablesShielded proves the SDK's own framework tables are
// never tenant-write-guarded (which would recurse / self-block), while domain tables
// are not shielded.
func TestWriteGuard_FrameworkTablesShielded(t *testing.T) {
	framework := []string{"tenant_fence", "tenant_event_seq", "tenant_event_policy", "outbox", "outbox_dispatch_cursor", "outbox_dead_letter", "idempotency_markers"}
	for _, tbl := range framework {
		if !gormtx.IsFrameworkTableForTest(tbl) {
			t.Errorf("framework table %q must be shielded from the write-guard", tbl)
		}
		// Namespaced (prefix) forms too.
		if !gormtx.IsFrameworkTableForTest("ord_" + tbl) {
			t.Errorf("namespaced framework table ord_%q must be shielded", tbl)
		}
	}
	if gormtx.IsFrameworkTableForTest("widgets") {
		t.Error("a domain table must NOT be shielded")
	}
}

// TestWriteGuard_EndToEnd_NoToken_Allows proves the registered callback ALLOWS a
// tenant-scoped write when ctx carries no admission token (service not cell-routed /
// not yet adopted): a sealed fence does not block a never-fenced writer.
func TestWriteGuard_EndToEnd_NoToken_Allows(t *testing.T) {
	db := openFenceDB(t, "fence_e2e_notoken")
	if err := gormtx.InstallTenantWriteGuard(db); err != nil {
		t.Fatalf("install guard: %v", err)
	}
	f := gormtx.NewGormFencer(db)
	ctx := context.Background()
	// Seal the tenant — but the writer has NO token, so it is allowed (fail-open).
	if err := f.SetOwner(ctx, "acme", "cell-a", 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Seal(ctx, "acme", 2); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Create(&widgetRow{ID: "w1", AccountID: "acme", Name: "n"}).Error; err != nil {
		t.Fatalf("no-token write must be allowed even when sealed, got %v", err)
	}
}

// TestWriteGuard_EndToEnd_TokenMismatch_Rejects proves the registered callback
// REJECTS a tenant-scoped write whose admission token does not match the fence. The
// token is stamped on ctx by the cells routing interceptor (the only producer of an
// admission token) so this exercises the real cell-routed path.
func TestWriteGuard_EndToEnd_TokenMismatch_Rejects(t *testing.T) {
	db := openFenceDB(t, "fence_e2e_mismatch")
	if err := gormtx.InstallTenantWriteGuard(db); err != nil {
		t.Fatalf("install guard: %v", err)
	}
	f := gormtx.NewGormFencer(db)
	bg := context.Background()
	// Fence owns "acme" on cell-a@1.
	if err := f.SetOwner(bg, "acme", "cell-a", 1); err != nil {
		t.Fatal(err)
	}

	// A writer admitted on cell-b@1 (wrong cell) gets a token via the interceptor.
	ctx := admittedContext(t, "cell-b", "acme", 1)
	err := db.WithContext(ctx).Create(&widgetRow{ID: "w2", AccountID: "acme", Name: "n"}).Error
	if !errors.Is(err, gormtx.ErrTenantFenced) {
		t.Fatalf("a mismatched-cell admitted write must be rejected with ErrTenantFenced, got %v", err)
	}
	// A rejected write must leave NO row (the guard aborts the create).
	var n int64
	db.WithContext(bg).Model(&widgetRow{}).Where("id = ?", "w2").Count(&n)
	if n != 0 {
		t.Fatalf("a fenced write must persist no row, found %d", n)
	}

	// A writer admitted on cell-a@1 (matching owner) is allowed.
	ctxOK := admittedContext(t, "cell-a", "acme", 1)
	if err := db.WithContext(ctxOK).Create(&widgetRow{ID: "w3", AccountID: "acme", Name: "n"}).Error; err != nil {
		t.Fatalf("a matching admitted write must be allowed, got %v", err)
	}
}

// TestWriteGuard_EndToEnd_NoAccountIDField_Skips proves the guard skips a model with
// no account_id field even under a cell-routed (token-bearing) context.
func TestWriteGuard_EndToEnd_NoAccountIDField_Skips(t *testing.T) {
	db := openFenceDB(t, "fence_e2e_noacct")
	if err := gormtx.InstallTenantWriteGuard(db); err != nil {
		t.Fatalf("install guard: %v", err)
	}
	ctx := admittedContext(t, "cell-a", "acme", 1)
	if err := db.WithContext(ctx).Create(&gizmoRow{ID: "g1", Name: "n"}).Error; err != nil {
		t.Fatalf("a model with no account_id must never be guarded, got %v", err)
	}
}
