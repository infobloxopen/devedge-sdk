package cells_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/cells"
)

// ---- fake routing table for unreachable/error scenarios ---------------------

// errTable is a fake RoutingTable whose Get always returns a configurable error
// (not ErrNoRoute) and whose Watch works via an in-memory channel. Used to test
// the Stale=true fail-safe path in the Router.
type errTable struct {
	getErr  error
	watchCh chan cells.RouteEvent
}

func newErrTable(getErr error) *errTable {
	return &errTable{
		getErr:  getErr,
		watchCh: make(chan cells.RouteEvent, 64),
	}
}

func (e *errTable) Get(_ context.Context, _ string) (cells.TenantRoute, error) {
	return cells.TenantRoute{}, e.getErr
}

func (e *errTable) CompareAndSet(_ context.Context, _, _ cells.TenantRoute) error {
	return errors.New("errTable: CompareAndSet not supported")
}

func (e *errTable) Watch(ctx context.Context) (<-chan cells.RouteEvent, error) {
	ch := make(chan cells.RouteEvent, 64)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// ---- pollResolve polls until the resolver returns a matching decision --------

func pollResolve(t *testing.T, router *cells.Router, tenant string, match func(cells.Decision) bool) cells.Decision {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d := router.Resolve(context.Background(), tenant)
		if match(d) {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pollResolve: condition not met within 2s for tenant %q", tenant)
	return cells.Decision{}
}

// ---- tests ------------------------------------------------------------------

func TestRouter_UnknownTenant_DefaultDecision(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dec := router.Resolve(context.Background(), "no-such-tenant")
	if !dec.IsDefault {
		t.Error("unknown tenant: expected IsDefault=true")
	}
	if dec.Known {
		t.Error("unknown tenant: expected Known=false")
	}
	if !dec.AdmitNew {
		t.Error("unknown tenant: expected AdmitNew=true")
	}
	if dec.Cell != cells.DefaultCellID {
		t.Errorf("unknown tenant: expected Cell=%q, got %q", cells.DefaultCellID, dec.Cell)
	}
}

func TestRouter_KnownActiveRoute(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	route := activeRoute("t1", "cell-a", 7)
	mustCAS(t, tbl, cells.TenantRoute{}, route)

	dec := pollResolve(t, router, "t1", func(d cells.Decision) bool {
		return d.Known && d.Cell == "cell-a"
	})

	if dec.Cell != "cell-a" {
		t.Errorf("expected Cell=cell-a, got %q", dec.Cell)
	}
	if dec.RouteEpoch != 7 {
		t.Errorf("expected RouteEpoch=7, got %d", dec.RouteEpoch)
	}
	if !dec.AdmitNew {
		t.Error("ACTIVE route: expected AdmitNew=true")
	}
	if dec.IsDefault {
		t.Error("ACTIVE route: expected IsDefault=false")
	}
	if !dec.Known {
		t.Error("ACTIVE route: expected Known=true")
	}
}

func TestRouter_MovingRoute_AdmitNewFalse(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	r1 := activeRoute("t2", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)
	// Wait for ACTIVE to appear in the router cache before writing QUIESCING.
	// This prevents the watch loop from processing event 1 (ACTIVE) after event 2 (QUIESCING).
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

	dec := pollResolve(t, router, "t2", func(d cells.Decision) bool {
		return !d.AdmitNew
	})

	if dec.AdmitNew {
		t.Error("QUIESCING route: expected AdmitNew=false")
	}
	if dec.IsDefault {
		t.Error("QUIESCING route: expected IsDefault=false")
	}
}

func TestRouter_TableUnreachableOnMiss_StaleDefaultDecision(t *testing.T) {
	getErr := errors.New("simulated table unreachable")
	tbl := newErrTable(getErr)
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dec := router.Resolve(context.Background(), "mystery-tenant")
	if !dec.Stale {
		t.Error("unreachable table + miss: expected Stale=true")
	}
	if !dec.IsDefault {
		t.Error("unreachable table + miss: expected IsDefault=true")
	}
}

func TestRouter_WithDefaultCell(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl, cells.WithDefaultCell("my-default"))
	if router.DefaultCell() != "my-default" {
		t.Errorf("expected DefaultCell=my-default, got %q", router.DefaultCell())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dec := router.Resolve(context.Background(), "some-unknown-tenant")
	if dec.Cell != "my-default" {
		t.Errorf("expected Cell=my-default for unknown tenant, got %q", dec.Cell)
	}
}

func TestRouter_WithDefaultCell_EmptyIgnored(t *testing.T) {
	tbl := cells.NewMemTable()
	// Empty string should be ignored; defaultCell stays "default".
	router := cells.NewRouter(tbl, cells.WithDefaultCell(""))
	if router.DefaultCell() != cells.DefaultCellID {
		t.Errorf("empty WithDefaultCell should be ignored; got %q", router.DefaultCell())
	}
}

func TestRouter_Health_ErrorBeforeStart_OkAfterStart(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)

	// Before Start, Check should return an error.
	if err := router.Check(context.Background()); err == nil {
		t.Error("expected Check() error before Start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// After Start, Check should return nil.
	if err := router.Check(context.Background()); err != nil {
		t.Errorf("expected Check() nil after Start, got: %v", err)
	}

	cancel()
}

func TestRouter_Health_Name(t *testing.T) {
	router := cells.NewRouter(cells.NewMemTable())
	if got := router.Name(); got != "cell-router" {
		t.Errorf("expected Name()=cell-router, got %q", got)
	}
}

func TestRouter_Start_WatchPropagatesAfterCreation(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Create the route after Start; the watch should propagate it.
	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("late", "cell-z", 3))

	dec := pollResolve(t, router, "late", func(d cells.Decision) bool {
		return d.Known && d.Cell == "cell-z"
	})
	if dec.RouteEpoch != 3 {
		t.Errorf("expected RouteEpoch=3, got %d", dec.RouteEpoch)
	}
}

func TestRouter_EmptyTenant_PassThrough(t *testing.T) {
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dec := router.Resolve(context.Background(), "")
	// Empty tenant should resolve as default.
	if !dec.IsDefault {
		t.Error("empty tenant: expected IsDefault=true")
	}
}

func TestRouter_WithFreshness_StalesAgedEntry(t *testing.T) {
	tbl := cells.NewMemTable()
	// Very short freshness — entries older than 1ms are stale.
	router := cells.NewRouter(tbl, cells.WithFreshness(1*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("t-fresh", "cell-a", 1))
	pollResolve(t, router, "t-fresh", func(d cells.Decision) bool { return d.Known })

	// Wait for entry to age.
	time.Sleep(10 * time.Millisecond)

	dec := router.Resolve(context.Background(), "t-fresh")
	if !dec.Stale {
		t.Error("expected Stale=true for aged cache entry")
	}
}
