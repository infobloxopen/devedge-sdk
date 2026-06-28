package cells_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/cells"
)

// pollUntil polls fn until it returns true or the timeout elapses.
func pollUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// runUnary drives the interceptor with a fake handler and returns the error (if any).
func runUnary(
	interceptor interface {
		Handle(ctx context.Context, req any, handler func(context.Context, any) (any, error)) (any, error)
	},
	ctx context.Context,
	tenantID string,
) error {
	// We build the interceptor above the call site, so just call the raw function.
	panic("use callCellInterceptor directly")
}

// callCellInterceptor calls the cell unary interceptor with the given tenant.
func callCellInterceptor(
	t *testing.T,
	router *cells.Router,
	gr *cells.GateRegistry,
	ctx context.Context,
	tenantID string,
) error {
	t.Helper()
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return tenantID }),
	)
	_, err := interceptor(ctx, "req", fakeUnaryInfo(), func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	})
	return err
}

// ---- Integration test -------------------------------------------------------

// TestIntegration_FullMoveLifecycle drives a complete tenant move from cell-a to
// cell-b through the MemTable + Router + GateRegistry + GateController stack,
// entirely in-memory with no external dependencies.
func TestIntegration_FullMoveLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ── 1. Build the shared infrastructure ───────────────────────────────────

	tbl := cells.NewMemTable()

	// Router.
	router := cells.NewRouter(tbl)
	if err := router.Start(ctx); err != nil {
		t.Fatalf("router.Start: %v", err)
	}

	// GateRegistry + GateController for cell-a.
	grA := cells.NewGateRegistry("cell-a", "inst-1")
	ctrlA := cells.NewGateController(grA, tbl, cells.DefaultCellID)
	if err := ctrlA.Start(ctx); err != nil {
		t.Fatalf("ctrlA.Start: %v", err)
	}

	// ── 2. CAS-create: tenant "t1" ACTIVE on "cell-a" at epoch 1 ─────────────

	r1 := cells.TenantRoute{
		TenantID:   "t1",
		RouteEpoch: 1,
		ActiveCell: "cell-a",
		State:      cells.StateActive,
	}
	if err := tbl.CompareAndSet(ctx, cells.TenantRoute{}, r1); err != nil {
		t.Fatalf("create t1: %v", err)
	}

	// Wait for Router and gate-a to see the route.
	pollUntil(t, 2*time.Second, func() bool {
		d := router.Resolve(ctx, "t1")
		return d.Known && d.Cell == "cell-a" && d.AdmitNew
	})
	pollUntil(t, 2*time.Second, func() bool {
		_, err := grA.TryEnter("t1", 1)
		if err == nil {
			// Immediately leave so we don't leak a token.
			tok, _ := grA.TryEnter("t1", 1)
			grA.Leave(tok)
			return true
		}
		return false
	})

	// Interceptor should admit the call.
	if err := callCellInterceptor(t, router, grA, ctx, "t1"); err != nil {
		t.Fatalf("step 2: interceptor rejected call on ACTIVE tenant: %v", err)
	}

	// ── 3. CAS-update: QUIESCING (source=cell-a, barrierEpoch=2) ─────────────

	// Wait for router to cache ACTIVE before writing QUIESCING to avoid watch ordering race.
	pollUntil(t, 2*time.Second, func() bool {
		d := router.Resolve(ctx, "t1")
		return d.Known && d.AdmitNew
	})

	rQ := cells.TenantRoute{
		TenantID:     "t1",
		RouteEpoch:   2,
		ActiveCell:   "cell-a",
		SourceCell:   "cell-a",
		TargetCell:   "cell-b",
		State:        cells.StateQuiescing,
		BarrierEpoch: 2,
	}
	if err := tbl.CompareAndSet(ctx, r1, rQ); err != nil {
		t.Fatalf("quiescing CAS: %v", err)
	}

	// Wait for Router to see QUIESCING.
	pollUntil(t, 2*time.Second, func() bool {
		return !router.Resolve(ctx, "t1").AdmitNew
	})

	// Interceptor now rejects with Unavailable.
	err := callCellInterceptor(t, router, grA, ctx, "t1")
	if status.Code(err) != codes.Unavailable {
		t.Errorf("step 3: expected Unavailable for moving tenant, got %v (code=%v)", err, status.Code(err))
	}

	// Gate-a should also deny TryEnter.
	pollUntil(t, 2*time.Second, func() bool {
		_, gateErr := grA.TryEnter("t1", 1)
		return errors.Is(gateErr, cells.ErrTenantDraining)
	})

	// ── 4. CAS-commit: ACTIVE on "cell-b" at epoch 3 ─────────────────────────

	// Wait for router to cache QUIESCING before committing to cell-b.
	pollUntil(t, 2*time.Second, func() bool {
		return !router.Resolve(ctx, "t1").AdmitNew
	})

	r3 := cells.TenantRoute{
		TenantID:   "t1",
		RouteEpoch: 3,
		ActiveCell: "cell-b",
		State:      cells.StateActive,
	}
	if err := tbl.CompareAndSet(ctx, rQ, r3); err != nil {
		t.Fatalf("commit CAS: %v", err)
	}

	// Gate-a should now be closed (tenant is on cell-b).
	pollUntil(t, 2*time.Second, func() bool {
		_, gateErr := grA.TryEnter("t1", 3)
		return errors.Is(gateErr, cells.ErrTenantDraining)
	})

	// ── 5. cell-b registry + controller ──────────────────────────────────────

	grB := cells.NewGateRegistry("cell-b", "inst-9")
	ctrlB := cells.NewGateController(grB, tbl, cells.DefaultCellID)
	if err := ctrlB.Start(ctx); err != nil {
		t.Fatalf("ctrlB.Start: %v", err)
	}

	// Router should now route t1 to cell-b.
	pollUntil(t, 2*time.Second, func() bool {
		d := router.Resolve(ctx, "t1")
		return d.Cell == "cell-b" && d.AdmitNew
	})

	// Build a cell-b-scoped interceptor.
	interceptorB := cells.UnaryServerInterceptor(
		cells.NewRouter(tbl, cells.WithDefaultCell("cell-b")),
		grB,
		cells.WithTenantFunc(func(context.Context) string { return "t1" }),
	)
	// Start the cell-b router.
	routerB := cells.NewRouter(tbl)
	if err := routerB.Start(ctx); err != nil {
		t.Fatalf("routerB.Start: %v", err)
	}
	pollUntil(t, 2*time.Second, func() bool {
		d := routerB.Resolve(ctx, "t1")
		return d.Cell == "cell-b" && d.AdmitNew
	})

	_ = interceptorB // tested below via routerB + grB

	// grB should open the gate at epoch 3 (via the controller watch).
	pollUntil(t, 2*time.Second, func() bool {
		tok, err := grB.TryEnter("t1", 3)
		if err == nil {
			grB.Leave(tok)
			return true
		}
		return false
	})

	// Interceptor with routerB + grB must admit t1.
	interceptorB2 := cells.UnaryServerInterceptor(routerB, grB,
		cells.WithTenantFunc(func(context.Context) string { return "t1" }),
	)
	_, admitErr := interceptorB2(ctx, "req", fakeUnaryInfo(), func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	})
	if admitErr != nil {
		t.Fatalf("step 5: cell-b interceptor rejected t1 at epoch 3: %v", admitErr)
	}

	// ── 6. Stale epoch assertion: cell-a gate (at epoch 1) rejects epoch 3 ───

	// grA's gate for t1 is now closed (gateClosed state), so TryEnter returns ErrTenantDraining.
	// For a stale-epoch test: open a fresh registry at epoch 1, then advance to 5 via Open,
	// then try to enter at 1 (the old epoch).
	grStale := cells.NewGateRegistry("cell-x", "inst-stale")
	grStale.Open("ts", 5)
	_, staleErr := grStale.TryEnter("ts", 1)
	if !errors.Is(staleErr, cells.ErrStaleRouteEpoch) {
		t.Errorf("stale epoch assertion: expected ErrStaleRouteEpoch, got %v", staleErr)
	}
}

// TestIntegration_GateController_DeleteRoute_RevertsToDefault proves the
// carried-forward hard requirement "turn off a cell → tenants instantly revert to
// the default cell" even when the default cell's gate had advanced to a non-zero
// epoch. On delete the controller Resets (forgets) the gate so the tenant is
// re-admitted at epoch 0 — Open alone would be a monotonic no-op and wrongly keep
// rejecting at the stale epoch.
func TestIntegration_GateController_DeleteRoute_RevertsToDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tbl := cells.NewMemTable()
	// The tenant is served ON the default cell at a non-zero epoch, then its route
	// is deleted: the default cell's gate is at epoch 5 and must revert to admit.
	grDefault := cells.NewGateRegistry(cells.DefaultCellID, "inst-def")
	ctrl := cells.NewGateController(grDefault, tbl, cells.DefaultCellID)
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("ctrl.Start: %v", err)
	}

	route := cells.TenantRoute{
		TenantID:   "td",
		RouteEpoch: 5,
		ActiveCell: cells.DefaultCellID,
		State:      cells.StateActive,
	}
	if err := tbl.CompareAndSet(ctx, cells.TenantRoute{}, route); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Controller observes the route and opens the default-cell gate at epoch 5:
	// TryEnter at 5 succeeds, and at 0 it is a stale-epoch conflict (proving the
	// gate really advanced past 0).
	pollUntil(t, 2*time.Second, func() bool {
		tok, err := grDefault.TryEnter("td", 5)
		if err == nil {
			grDefault.Leave(tok)
			return true
		}
		return false
	})
	if _, err := grDefault.TryEnter("td", 0); !errors.Is(err, cells.ErrStaleRouteEpoch) {
		t.Fatalf("pre-delete: TryEnter(td,0) = %v, want ErrStaleRouteEpoch", err)
	}

	// Delete the route → the tenant reverts to the default cell, which must admit
	// again at epoch 0 (this is the regression the Reset fix addresses).
	if err := tbl.Delete(ctx, "td"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	pollUntil(t, 2*time.Second, func() bool {
		tok, err := grDefault.TryEnter("td", 0)
		if err == nil {
			grDefault.Leave(tok)
			return true
		}
		return false
	})
}

// TestIntegration_MultiTenant_Isolation ensures that draining one tenant does
// not affect another tenant on the same instance.
func TestIntegration_MultiTenant_Isolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	if err := router.Start(ctx); err != nil {
		t.Fatalf("router.Start: %v", err)
	}
	gr := cells.NewGateRegistry("cell-a", "inst-1")
	ctrl := cells.NewGateController(gr, tbl, cells.DefaultCellID)
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("ctrl.Start: %v", err)
	}

	// Two tenants both on cell-a.
	for _, id := range []string{"ta", "tb"} {
		r := activeRoute(id, "cell-a", 1)
		if err := tbl.CompareAndSet(ctx, cells.TenantRoute{}, r); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// Wait for both to be known.
	for _, id := range []string{"ta", "tb"} {
		id := id
		pollUntil(t, 2*time.Second, func() bool {
			return router.Resolve(ctx, id).Known
		})
	}

	// Drain only "ta".
	rA1 := activeRoute("ta", "cell-a", 1)
	// Wait for ACTIVE to be cached for "ta" before writing QUIESCING.
	pollUntil(t, 2*time.Second, func() bool {
		return router.Resolve(ctx, "ta").Known && router.Resolve(ctx, "ta").AdmitNew
	})

	rAQ := cells.TenantRoute{
		TenantID:     "ta",
		RouteEpoch:   2,
		ActiveCell:   "cell-a",
		SourceCell:   "cell-a",
		State:        cells.StateQuiescing,
		BarrierEpoch: 2,
	}
	if err := tbl.CompareAndSet(ctx, rA1, rAQ); err != nil {
		t.Fatalf("quiesce ta: %v", err)
	}
	pollUntil(t, 2*time.Second, func() bool {
		return !router.Resolve(ctx, "ta").AdmitNew
	})

	// "ta" should be rejected.
	if err := callCellInterceptor(t, router, gr, ctx, "ta"); status.Code(err) != codes.Unavailable {
		t.Errorf("ta: expected Unavailable, got %v", err)
	}

	// "tb" should still be admitted.
	if err := callCellInterceptor(t, router, gr, ctx, "tb"); err != nil {
		t.Errorf("tb: expected nil (admitted), got %v", err)
	}
}
