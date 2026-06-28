package cells_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/cells"
)

// ---- helpers ----------------------------------------------------------------

func mustCAS(t *testing.T, tbl *cells.MemTable, expect, next cells.TenantRoute) {
	t.Helper()
	if err := tbl.CompareAndSet(context.Background(), expect, next); err != nil {
		t.Fatalf("CompareAndSet: unexpected error: %v", err)
	}
}

func activeRoute(tenantID, cellID string, epoch uint64) cells.TenantRoute {
	return cells.TenantRoute{
		TenantID:   tenantID,
		RouteEpoch: epoch,
		ActiveCell: cellID,
		State:      cells.StateActive,
	}
}

// ---- Get --------------------------------------------------------------------

func TestMemTable_Get_ErrNoRoute_WhenAbsent(t *testing.T) {
	tbl := cells.NewMemTable()
	_, err := tbl.Get(context.Background(), "nobody")
	if !errors.Is(err, cells.ErrNoRoute) {
		t.Fatalf("expected ErrNoRoute, got %v", err)
	}
}

func TestMemTable_Get_ReturnsStoredRoute(t *testing.T) {
	tbl := cells.NewMemTable()
	route := activeRoute("t1", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, route)

	got, err := tbl.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if got.TenantID != route.TenantID || got.ActiveCell != route.ActiveCell || got.RouteEpoch != route.RouteEpoch {
		t.Errorf("Get: got %+v, want %+v", got, route)
	}
}

// ---- CompareAndSet: CREATE ---------------------------------------------------

func TestMemTable_CAS_Create_SucceedsWithZeroExpect(t *testing.T) {
	tbl := cells.NewMemTable()
	route := activeRoute("t-new", "cell-a", 1)
	if err := tbl.CompareAndSet(context.Background(), cells.TenantRoute{}, route); err != nil {
		t.Fatalf("create CAS unexpected error: %v", err)
	}
}

func TestMemTable_CAS_Create_ConflictWhenExpectNonZero(t *testing.T) {
	tbl := cells.NewMemTable()
	// Tenant absent but expect is non-zero → conflict.
	badExpect := activeRoute("t-miss", "cell-a", 0)
	next := activeRoute("t-miss", "cell-a", 1)
	err := tbl.CompareAndSet(context.Background(), badExpect, next)
	if !errors.Is(err, cells.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict, got %v", err)
	}
}

func TestMemTable_CAS_Create_ConflictWhenTenantAlreadyExists(t *testing.T) {
	tbl := cells.NewMemTable()
	route := activeRoute("t1", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, route)

	// Trying to create again with zero expect on an existing tenant → conflict.
	err := tbl.CompareAndSet(context.Background(), cells.TenantRoute{}, activeRoute("t1", "cell-a", 2))
	if !errors.Is(err, cells.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict for duplicate create, got %v", err)
	}
}

func TestMemTable_CAS_EmptyTenantID_ConflictsAlways(t *testing.T) {
	tbl := cells.NewMemTable()
	err := tbl.CompareAndSet(context.Background(), cells.TenantRoute{}, cells.TenantRoute{})
	if !errors.Is(err, cells.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict when next.TenantID is empty, got %v", err)
	}
}

// ---- CompareAndSet: UPDATE ---------------------------------------------------

func TestMemTable_CAS_Update_SucceedsOnMatch(t *testing.T) {
	tbl := cells.NewMemTable()
	r1 := activeRoute("t1", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)

	r2 := cells.TenantRoute{
		TenantID:   "t1",
		RouteEpoch: 2,
		ActiveCell: "cell-a",
		State:      cells.StateQuiescing,
	}
	mustCAS(t, tbl, r1, r2)

	got, _ := tbl.Get(context.Background(), "t1")
	if got.State != cells.StateQuiescing || got.RouteEpoch != 2 {
		t.Errorf("Update: got %+v", got)
	}
}

func TestMemTable_CAS_Update_ConflictOnEpochMismatch(t *testing.T) {
	tbl := cells.NewMemTable()
	r1 := activeRoute("t1", "cell-a", 5)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)

	badExpect := activeRoute("t1", "cell-a", 4) // wrong epoch
	next := activeRoute("t1", "cell-a", 6)
	err := tbl.CompareAndSet(context.Background(), badExpect, next)
	if !errors.Is(err, cells.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict on epoch mismatch, got %v", err)
	}
}

func TestMemTable_CAS_Update_ConflictOnStateMismatch(t *testing.T) {
	tbl := cells.NewMemTable()
	r1 := activeRoute("t1", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)

	wrongStateExpect := cells.TenantRoute{TenantID: "t1", RouteEpoch: 1, State: cells.StateQuiescing}
	next := activeRoute("t1", "cell-a", 2)
	err := tbl.CompareAndSet(context.Background(), wrongStateExpect, next)
	if !errors.Is(err, cells.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict on state mismatch, got %v", err)
	}
}

func TestMemTable_CAS_Update_ErrEpochRegression(t *testing.T) {
	tbl := cells.NewMemTable()
	r1 := activeRoute("t1", "cell-a", 5)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)

	lower := activeRoute("t1", "cell-a", 4)
	err := tbl.CompareAndSet(context.Background(), r1, lower)
	if !errors.Is(err, cells.ErrEpochRegression) {
		t.Fatalf("expected ErrEpochRegression, got %v", err)
	}
}

// ---- Delete -----------------------------------------------------------------

func TestMemTable_Delete_Idempotent(t *testing.T) {
	tbl := cells.NewMemTable()
	// Delete on absent tenant must not error.
	if err := tbl.Delete(context.Background(), "nobody"); err != nil {
		t.Fatalf("Delete absent: unexpected error: %v", err)
	}

	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("t1", "cell-a", 1))
	if err := tbl.Delete(context.Background(), "t1"); err != nil {
		t.Fatalf("Delete existing: unexpected error: %v", err)
	}
	if err := tbl.Delete(context.Background(), "t1"); err != nil {
		t.Fatalf("Delete again (idempotent): unexpected error: %v", err)
	}
	// After delete, Get should return ErrNoRoute.
	_, err := tbl.Get(context.Background(), "t1")
	if !errors.Is(err, cells.ErrNoRoute) {
		t.Fatalf("after Delete, Get should return ErrNoRoute, got %v", err)
	}
}

func TestMemTable_Delete_EmitsDeleteEvent(t *testing.T) {
	tbl := cells.NewMemTable()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := tbl.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	mustCAS(t, tbl, cells.TenantRoute{}, activeRoute("t1", "cell-a", 1))
	// Drain the create event.
	drainEvent(t, ch, ctx)

	if err := tbl.Delete(ctx, "t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	select {
	case ev := <-ch:
		if !ev.Deleted {
			t.Errorf("expected Deleted=true, got %+v", ev)
		}
		if ev.TenantID != "t1" {
			t.Errorf("expected TenantID=t1, got %q", ev.TenantID)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for delete event")
	}
}

// ---- Watch ------------------------------------------------------------------

func TestMemTable_Watch_DeliversCreateEvent(t *testing.T) {
	tbl := cells.NewMemTable()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := tbl.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	route := activeRoute("t1", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, route)

	select {
	case ev := <-ch:
		if ev.Deleted {
			t.Errorf("unexpected Deleted=true")
		}
		if ev.TenantID != "t1" {
			t.Errorf("expected TenantID t1, got %q", ev.TenantID)
		}
		if ev.Route.RouteEpoch != 1 {
			t.Errorf("expected epoch 1, got %d", ev.Route.RouteEpoch)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for create event")
	}
}

func TestMemTable_Watch_DeliversUpdateEvent(t *testing.T) {
	tbl := cells.NewMemTable()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r1 := activeRoute("t1", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, r1)

	ch, err := tbl.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	r2 := activeRoute("t1", "cell-a", 2)
	mustCAS(t, tbl, r1, r2)

	select {
	case ev := <-ch:
		if ev.Deleted {
			t.Errorf("unexpected Deleted=true for update")
		}
		if ev.Route.RouteEpoch != 2 {
			t.Errorf("expected epoch 2 for update, got %d", ev.Route.RouteEpoch)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for update event")
	}
}

func TestMemTable_Watch_ChannelClosesOnCancel(t *testing.T) {
	tbl := cells.NewMemTable()
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := tbl.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	cancel()

	// Give the cleanup goroutine time to close the channel.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed, test passes
			}
		case <-deadline:
			t.Fatal("watch channel not closed after context cancel")
		}
	}
}

func TestMemTable_Watch_RevisionMonotonicallyIncreases(t *testing.T) {
	tbl := cells.NewMemTable()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := tbl.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	tenants := []string{"ta", "tb", "tc"}
	for _, id := range tenants {
		mustCAS(t, tbl, cells.TenantRoute{}, activeRoute(id, "cell-a", 1))
	}

	var lastRev uint64
	for i := 0; i < len(tenants); i++ {
		select {
		case ev := <-ch:
			if ev.Revision <= lastRev {
				t.Errorf("revision not monotonic: got %d after %d", ev.Revision, lastRev)
			}
			lastRev = ev.Revision
		case <-ctx.Done():
			t.Fatal("timeout waiting for events")
		}
	}
}

// ---- Concurrency ------------------------------------------------------------

func TestMemTable_ConcurrentCAS_DistinctTenants(t *testing.T) {
	const n = 50
	tbl := cells.NewMemTable()
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenantID := "tenant-concurrent-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
			errs[i] = tbl.CompareAndSet(ctx, cells.TenantRoute{}, activeRoute(tenantID, "cell-a", 1))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
}

func TestMemTable_ConcurrentCAS_ContentedSingleTenant_ExactlyOneWins(t *testing.T) {
	tbl := cells.NewMemTable()
	ctx := context.Background()

	// Create the initial route.
	r0 := activeRoute("t-race", "cell-a", 1)
	mustCAS(t, tbl, cells.TenantRoute{}, r0)

	// N goroutines all try to CAS from (epoch=1,ACTIVE) → (epoch=2,ACTIVE).
	const n = 20
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := tbl.CompareAndSet(ctx,
				activeRoute("t-race", "cell-a", 1),
				activeRoute("t-race", "cell-a", 2),
			)
			if err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Errorf("expected exactly 1 CAS winner, got %d", got)
	}
}

// drainEvent reads one event from ch (discarding it) within the ctx timeout.
func drainEvent(t *testing.T, ch <-chan cells.RouteEvent, ctx context.Context) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("timeout draining event")
	}
}
