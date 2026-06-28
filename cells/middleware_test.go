package cells_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/cells"
	mw "github.com/infobloxopen/devedge-sdk/middleware"
)

// ---- DefaultIsMutating ------------------------------------------------------

func TestDefaultIsMutating_ReadMethods(t *testing.T) {
	reads := []string{
		"/p.S/GetX",
		"/p.S/ListX",
		"/p.S/BatchGetX",
		"/p.S/SearchX",
		"/p.S/WatchX",
		"/p.S/LookupFoo",
		"/p.S/ReadThing",
		"/p.S/ExportAll",
		"/p.S/QueryUsers",
		"/p.S/CheckStatus",
		"/p.S/StreamEvents",
	}
	for _, m := range reads {
		if cells.DefaultIsMutating(m) {
			t.Errorf("DefaultIsMutating(%q) = true, want false (read method)", m)
		}
	}
}

func TestDefaultIsMutating_WriteMethods(t *testing.T) {
	writes := []string{
		"/p.S/CreateX",
		"/p.S/UpdateX",
		"/p.S/DeleteX",
		"/p.S/BatchCreateX",
		"/p.S/PatchFoo",
		"/p.S/DoSomething",
	}
	for _, m := range writes {
		if !cells.DefaultIsMutating(m) {
			t.Errorf("DefaultIsMutating(%q) = false, want true (mutating method)", m)
		}
	}
}

// ---- AdmissionTokenFromContext ----------------------------------------------

func TestAdmissionTokenFromContext_AbsentReturnsFalse(t *testing.T) {
	_, ok := cells.AdmissionTokenFromContext(context.Background())
	if ok {
		t.Error("expected ok=false for context without admission token")
	}
}

// ---- gRPC UnaryServerInterceptor helpers ------------------------------------

// fakeHandler records whether it was called and the context it received.
type fakeHandler struct {
	called bool
	ctx    context.Context
}

func (h *fakeHandler) handle(ctx context.Context, req any) (any, error) {
	h.called = true
	h.ctx = ctx
	return "ok", nil
}

// makeInterceptorSetup builds a Router + GateRegistry around a fresh MemTable.
func makeInterceptorSetup(t *testing.T, cellID string) (*cells.MemTable, *cells.Router, *cells.GateRegistry, context.Context, context.CancelFunc) {
	t.Helper()
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	if err := router.Start(ctx); err != nil {
		cancel()
		t.Fatalf("router.Start: %v", err)
	}
	gr := cells.NewGateRegistry(cellID, "inst-test")
	return tbl, router, gr, ctx, cancel
}

// callInterceptor drives the unary interceptor synchronously.
func callInterceptor(
	interceptor grpc.UnaryServerInterceptor,
	ctx context.Context,
	fullMethod string,
	handler grpc.UnaryHandler,
) (any, error) {
	return interceptor(ctx, "req", &grpc.UnaryServerInfo{FullMethod: fullMethod}, handler)
}

// tenantCtx builds a context carrying the tenant header as gRPC metadata,
// then runs TenantIDUnary so TenantIDFromContext works.
func tenantCtx(t *testing.T, tenantID string) context.Context {
	t.Helper()
	// TenantIDFromContext is populated by TenantIDUnary interceptor; since we're
	// calling the cell interceptor directly we inject using WithTenantFunc instead.
	return context.Background()
}

// ---- UnaryServerInterceptor tests -------------------------------------------

func TestUnaryInterceptor_EmptyTenant_PassThrough(t *testing.T) {
	tbl, router, gr, ctx, cancel := makeInterceptorSetup(t, "cell-a")
	defer cancel()
	_ = tbl

	handler := &fakeHandler{}
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return "" }),
	)
	_, err := callInterceptor(interceptor, ctx, "/svc/GetFoo", handler.handle)
	if err != nil {
		t.Fatalf("empty tenant: unexpected error: %v", err)
	}
	if !handler.called {
		t.Error("empty tenant: handler should be called (passthrough)")
	}
}

func TestUnaryInterceptor_TenantActiveOnCell_Admitted(t *testing.T) {
	tbl, router, gr, ctx, cancel := makeInterceptorSetup(t, "cell-a")
	defer cancel()

	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("t1", "cell-a", 1))
	pollResolve(t, router, "t1", func(d cells.Decision) bool { return d.Known })

	var tokenInCtx cells.AdmissionToken
	var tokenOk bool
	handler := func(hctx context.Context, req any) (any, error) {
		tokenInCtx, tokenOk = cells.AdmissionTokenFromContext(hctx)
		return "ok", nil
	}

	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return "t1" }),
	)
	_, err := callInterceptor(interceptor, ctx, "/svc/CreateFoo", handler)
	if err != nil {
		t.Fatalf("active tenant: unexpected error: %v", err)
	}
	if !tokenOk {
		t.Error("active tenant: AdmissionToken should be present in handler context")
	}
	if tokenInCtx.TenantID != "t1" {
		t.Errorf("expected token TenantID=t1, got %q", tokenInCtx.TenantID)
	}
	// After handler returns, inflight should be 0.
	if n := gr.Inflight("t1"); n != 0 {
		t.Errorf("expected inflight=0 after interceptor returns, got %d", n)
	}
}

func TestUnaryInterceptor_MovingTenant_Unavailable(t *testing.T) {
	tbl, router, gr, ctx, cancel := makeInterceptorSetup(t, "cell-a")
	defer cancel()

	r1 := activeRoute("t2", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)
	// Wait for ACTIVE to appear in router cache before writing QUIESCING.
	// This ensures the watch loop has processed event 1, so event 2 (QUIESCING)
	// won't be overwritten by a late event 1 write.
	pollResolve(t, router, "t2", func(d cells.Decision) bool { return d.Known && d.AdmitNew })

	quiescing := cells.TenantRoute{
		TenantID:   "t2",
		RouteEpoch: 2,
		ActiveCell: "cell-a",
		SourceCell: "cell-a",
		TargetCell: "cell-b",
		State:      cells.StateQuiescing,
	}
	mustCAS(t, tbl, r1, quiescing)
	pollResolve(t, router, "t2", func(d cells.Decision) bool { return !d.AdmitNew })

	handler := &fakeHandler{}
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return "t2" }),
	)
	_, err := callInterceptor(interceptor, ctx, "/svc/CreateFoo", handler.handle)
	if handler.called {
		t.Error("moving tenant: handler must NOT be called")
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("moving tenant: expected Unavailable, got %v", status.Code(err))
	}
}

func TestUnaryInterceptor_WrongCell_Unavailable(t *testing.T) {
	tbl, router, gr, ctx, cancel := makeInterceptorSetup(t, "cell-a")
	defer cancel()

	// Route points to "cell-other", not "cell-a".
	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("t3", "cell-other", 1))
	pollResolve(t, router, "t3", func(d cells.Decision) bool { return d.Known })

	handler := &fakeHandler{}
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return "t3" }),
	)
	_, err := callInterceptor(interceptor, ctx, "/svc/CreateFoo", handler.handle)
	if handler.called {
		t.Error("wrong cell: handler must NOT be called")
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("wrong cell: expected Unavailable, got %v", status.Code(err))
	}
}

func TestUnaryInterceptor_StaleRoute_Read_PassThrough(t *testing.T) {
	// Use an errTable so Resolve returns Stale=true, IsDefault=true, Cell=DefaultCellID.
	// The gate registry must be on the DefaultCellID so the wrong-cell check passes.
	getErr := errors.New("table down")
	tbl := newErrTable(getErr)
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Use DefaultCellID registry so dec.Cell == gates.CellID() (no wrong-cell rejection).
	gr := cells.NewGateRegistry(cells.DefaultCellID, "inst-1")
	handler := &fakeHandler{}
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return "mystery" }),
	)
	// GET (read) under Stale should pass through.
	_, err := callInterceptor(interceptor, ctx, "/svc/GetFoo", handler.handle)
	if err != nil {
		t.Fatalf("stale+read: unexpected error: %v", err)
	}
	if !handler.called {
		t.Error("stale+read: handler should be called")
	}
}

func TestUnaryInterceptor_StaleRoute_Write_Unavailable(t *testing.T) {
	getErr := errors.New("table down")
	tbl := newErrTable(getErr)
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// No gates — use nil GateRegistry to avoid wrong-cell check triggering first.
	// (Stale=true + isDefault → cell is DefaultCellID; gates.CellID() also DefaultCellID)
	gr := cells.NewGateRegistry(cells.DefaultCellID, "inst-1")
	handler := &fakeHandler{}
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return "mystery" }),
	)
	// POST (mutating) under Stale should be rejected.
	_, err := callInterceptor(interceptor, ctx, "/svc/CreateFoo", handler.handle)
	if handler.called {
		t.Error("stale+write: handler must NOT be called")
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("stale+write: expected Unavailable, got %v", status.Code(err))
	}
}

func TestUnaryInterceptor_GateStaleEpoch_Aborted(t *testing.T) {
	// Test gateErr maps ErrStaleRouteEpoch to Aborted.
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	gr := cells.NewGateRegistry("cell-a", "inst-1")
	// Create a route at epoch 1.
	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("t-epoch", "cell-a", 1))
	pollResolve(t, router, "t-epoch", func(d cells.Decision) bool { return d.Known })

	// Open the gate at a higher epoch (5) — so when TryEnter is called with epoch 1,
	// the gate will report ErrStaleRouteEpoch.
	gr.Open("t-epoch", 5)

	handler := &fakeHandler{}
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return "t-epoch" }),
	)
	_, err := callInterceptor(interceptor, ctx, "/svc/CreateFoo", handler.handle)
	if handler.called {
		t.Error("stale gate epoch: handler must NOT be called")
	}
	if status.Code(err) != codes.Aborted {
		t.Errorf("stale gate epoch: expected Aborted, got %v (err=%v)", status.Code(err), err)
	}
}

func TestUnaryInterceptor_DrainedGate_Unavailable(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	gr := cells.NewGateRegistry("cell-a", "inst-1")
	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("t-drain", "cell-a", 1))
	pollResolve(t, router, "t-drain", func(d cells.Decision) bool { return d.Known })

	// Drain the gate before calling the interceptor.
	gr.Reconcile(cells.TenantRoute{
		TenantID:     "t-drain",
		RouteEpoch:   2,
		ActiveCell:   "cell-a",
		SourceCell:   "cell-a",
		State:        cells.StateQuiescing,
		BarrierEpoch: 2,
	})

	handler := &fakeHandler{}
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return "t-drain" }),
	)
	// Note: the router still sees ACTIVE (we haven't updated the table), so AdmitNew=true,
	// but the gate is draining → TryEnter returns ErrTenantDraining → codes.Unavailable.
	_, err := callInterceptor(interceptor, ctx, "/svc/CreateFoo", handler.handle)
	if handler.called {
		t.Error("draining gate: handler must NOT be called")
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("draining gate: expected Unavailable, got %v", status.Code(err))
	}
}

// ---- HTTP middleware ---------------------------------------------------------

func TestHTTPMiddleware_MovingTenant_503WithRetryAfter(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	r1 := activeRoute("http-t1", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)
	// Wait for ACTIVE to be seen before writing QUIESCING, to avoid watch event ordering race.
	pollResolve(t, router, "http-t1", func(d cells.Decision) bool { return d.Known && d.AdmitNew })

	quiescing := cells.TenantRoute{
		TenantID:   "http-t1",
		RouteEpoch: 2,
		ActiveCell: "cell-a",
		SourceCell: "cell-a",
		State:      cells.StateQuiescing,
	}
	mustCAS(t, tbl, r1, quiescing)
	pollResolve(t, router, "http-t1", func(d cells.Decision) bool { return !d.AdmitNew })

	var nextCalled bool
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	})
	mwHandler := cells.HTTPMiddleware(router)(nextHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/resource", nil)
	req.Header.Set("account-id", "http-t1")
	w := httptest.NewRecorder()
	mwHandler.ServeHTTP(w, req)

	if nextCalled {
		t.Error("moving tenant: next handler must NOT be called")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("moving tenant: expected 503, got %d", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("moving tenant: expected Retry-After header")
	}
}

func TestHTTPMiddleware_StaleRead_PassThrough(t *testing.T) {
	tbl := newErrTable(errors.New("table down"))
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var nextCalled bool
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	})
	mwHandler := cells.HTTPMiddleware(router)(nextHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
	req.Header.Set("account-id", "mystery")
	w := httptest.NewRecorder()
	mwHandler.ServeHTTP(w, req)

	if !nextCalled {
		t.Error("stale+GET: next handler should be called")
	}
}

func TestHTTPMiddleware_StaleWrite_503(t *testing.T) {
	tbl := newErrTable(errors.New("table down"))
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var nextCalled bool
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	})
	mwHandler := cells.HTTPMiddleware(router)(nextHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/resource", nil)
	req.Header.Set("account-id", "mystery")
	w := httptest.NewRecorder()
	mwHandler.ServeHTTP(w, req)

	if nextCalled {
		t.Error("stale+POST: next handler must NOT be called")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("stale+POST: expected 503, got %d", w.Code)
	}
}

func TestHTTPMiddleware_Normal_CellIDHeaderSet(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("http-t2", "cell-a", 1))
	pollResolve(t, router, "http-t2", func(d cells.Decision) bool { return d.Known })

	var nextCalled bool
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	})
	mwHandler := cells.HTTPMiddleware(router)(nextHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/resource", nil)
	req.Header.Set("account-id", "http-t2")
	w := httptest.NewRecorder()
	mwHandler.ServeHTTP(w, req)

	if !nextCalled {
		t.Error("normal request: next handler should be called")
	}
	if cellHdr := w.Header().Get("cell-id"); cellHdr != "cell-a" {
		t.Errorf("expected cell-id header=cell-a, got %q", cellHdr)
	}
}

func TestHTTPMiddleware_EmptyTenant_PassThrough(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var nextCalled bool
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	})
	mwHandler := cells.HTTPMiddleware(router)(nextHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/resource", nil)
	// No account-id header → empty tenant → passthrough.
	w := httptest.NewRecorder()
	mwHandler.ServeHTTP(w, req)

	if !nextCalled {
		t.Error("empty tenant: next handler should be called")
	}
}

func TestHTTPMiddleware_WithRetryAfter(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	r1 := activeRoute("http-t3", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)
	// Wait for ACTIVE before writing QUIESCING (watch ordering).
	pollResolve(t, router, "http-t3", func(d cells.Decision) bool { return d.Known && d.AdmitNew })

	quiescing := cells.TenantRoute{
		TenantID:   "http-t3",
		RouteEpoch: 2,
		ActiveCell: "cell-a",
		SourceCell: "cell-a",
		State:      cells.StateQuiescing,
	}
	mustCAS(t, tbl, r1, quiescing)
	pollResolve(t, router, "http-t3", func(d cells.Decision) bool { return !d.AdmitNew })

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	mwHandler := cells.HTTPMiddleware(router,
		cells.WithRetryAfter(10*time.Second),
	)(nextHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/resource", nil)
	req.Header.Set("account-id", "http-t3")
	w := httptest.NewRecorder()
	mwHandler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra != "10" {
		t.Errorf("expected Retry-After=10, got %q", ra)
	}
}

// Ensure middleware package is used (TenantIDFromContext) to avoid import error.
var _ = mw.TenantIDFromContext
