package cells_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/cells"
)

// ---- helpers ----------------------------------------------------------------

func newRegistry(cellID string) *cells.GateRegistry {
	return cells.NewGateRegistry(cellID, "inst-test")
}

// ---- TryEnter ---------------------------------------------------------------

func TestGate_TryEnter_StrictlyIncreasingSeq(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	var prev uint64
	for i := 0; i < 5; i++ {
		tok, err := gr.TryEnter("t1", 1)
		if err != nil {
			t.Fatalf("TryEnter[%d]: unexpected error: %v", i, err)
		}
		if tok.AdmissionSeq <= prev {
			t.Errorf("AdmissionSeq not strictly increasing: got %d after %d", tok.AdmissionSeq, prev)
		}
		prev = tok.AdmissionSeq
		gr.Leave(tok)
	}
}

func TestGate_TryEnter_LazyOpenAtEpoch(t *testing.T) {
	gr := newRegistry("cell-a")
	// No explicit Open — gate is lazily created at epoch 0.
	tok, err := gr.TryEnter("lazy-tenant", 0)
	if err != nil {
		t.Fatalf("TryEnter lazy open: %v", err)
	}
	if tok.TenantID != "lazy-tenant" {
		t.Errorf("expected TenantID=lazy-tenant, got %q", tok.TenantID)
	}
	gr.Leave(tok)
}

func TestGate_TryEnter_ErrStaleRouteEpoch(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 5) // gate is at epoch 5

	// Try to enter at epoch 4 (lower than gate epoch).
	_, err := gr.TryEnter("t1", 4)
	if !errors.Is(err, cells.ErrStaleRouteEpoch) {
		t.Fatalf("expected ErrStaleRouteEpoch, got %v", err)
	}
}

func TestGate_TryEnter_ErrStaleRouteEpoch_HigherRequest(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 5) // gate is at epoch 5

	// Try to enter at epoch 6 (higher than gate epoch) — also stale from gate's perspective.
	_, err := gr.TryEnter("t1", 6)
	if !errors.Is(err, cells.ErrStaleRouteEpoch) {
		t.Fatalf("expected ErrStaleRouteEpoch for epoch mismatch (6 vs 5), got %v", err)
	}
}

func TestGate_TryEnter_ErrTenantDraining_AfterBeginDrain(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	// Use beginDrain via CloseForBarrier (which internally calls beginDrain logic).
	// We do it via Reconcile with a moving route.
	movingRoute := cells.TenantRoute{
		TenantID:     "t1",
		RouteEpoch:   2,
		ActiveCell:   "cell-a",
		SourceCell:   "cell-a",
		TargetCell:   "cell-b",
		State:        cells.StateQuiescing,
		BarrierEpoch: 2,
	}
	gr.Reconcile(movingRoute)

	_, err := gr.TryEnter("t1", 1)
	if !errors.Is(err, cells.ErrTenantDraining) {
		t.Fatalf("expected ErrTenantDraining after drain, got %v", err)
	}
}

func TestGate_TryEnter_ErrTenantDraining_AfterClose(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	// Close the gate (tenant moved to different cell).
	otherCellRoute := cells.TenantRoute{
		TenantID:   "t1",
		RouteEpoch: 2,
		ActiveCell: "cell-b", // different cell
		State:      cells.StateActive,
	}
	gr.Reconcile(otherCellRoute)

	_, err := gr.TryEnter("t1", 2)
	if !errors.Is(err, cells.ErrTenantDraining) {
		t.Fatalf("expected ErrTenantDraining after gate closed, got %v", err)
	}
}

// ---- Leave ------------------------------------------------------------------

func TestGate_Leave_DropsInflight(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	tok, err := gr.TryEnter("t1", 1)
	if err != nil {
		t.Fatalf("TryEnter: %v", err)
	}
	if n := gr.Inflight("t1"); n != 1 {
		t.Errorf("expected inflight=1, got %d", n)
	}

	gr.Leave(tok)
	if n := gr.Inflight("t1"); n != 0 {
		t.Errorf("expected inflight=0 after Leave, got %d", n)
	}
}

func TestGate_Leave_NoGate_NoOp(t *testing.T) {
	gr := newRegistry("cell-a")
	// Leaving a token for a tenant with no gate should not panic.
	gr.Leave(cells.AdmissionToken{TenantID: "ghost"})
}

// ---- CloseForBarrier --------------------------------------------------------

func TestGate_CloseForBarrier_DrainedTrue_WhenInflightReleased(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	tok, err := gr.TryEnter("t1", 1)
	if err != nil {
		t.Fatalf("TryEnter: %v", err)
	}

	// Release the token from a separate goroutine.
	go func() {
		time.Sleep(20 * time.Millisecond)
		gr.Leave(tok)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cut := gr.CloseForBarrier(ctx, "t1", 2)
	if !cut.Drained {
		t.Error("expected Drained=true")
	}
	if cut.Forced {
		t.Error("expected Forced=false when drained cleanly")
	}
}

func TestGate_CloseForBarrier_Forced_WhenCtxExpires(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	tok, err := gr.TryEnter("t1", 1)
	if err != nil {
		t.Fatalf("TryEnter: %v", err)
	}
	defer gr.Leave(tok) // release after test

	// Very short deadline — should force.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	cut := gr.CloseForBarrier(ctx, "t1", 2)
	if cut.Drained {
		t.Error("expected Drained=false when forced")
	}
	if !cut.Forced {
		t.Error("expected Forced=true when ctx expired with in-flight work")
	}
}

func TestGate_CloseForBarrier_NoHang_ShortDeadline(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	tok, _ := gr.TryEnter("t1", 1)
	defer gr.Leave(tok)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		gr.CloseForBarrier(ctx, "t1", 2)
		close(done)
	}()

	select {
	case <-done:
		// passed — CloseForBarrier returned within the deadline + some slack
	case <-time.After(500 * time.Millisecond):
		t.Fatal("CloseForBarrier hung beyond the ctx deadline")
	}
}

// ---- Open (monotonic) -------------------------------------------------------

func TestGate_Open_MonotonicEpoch(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 5)

	// Re-open at a lower epoch must be ignored.
	gr.Open("t1", 3)

	// TryEnter at epoch 5 should still work.
	tok, err := gr.TryEnter("t1", 5)
	if err != nil {
		t.Fatalf("TryEnter at epoch 5 after Open(3) ignored: %v", err)
	}
	gr.Leave(tok)

	// TryEnter at epoch 3 should fail (epoch mismatch — gate is at 5).
	_, err = gr.TryEnter("t1", 3)
	if !errors.Is(err, cells.ErrStaleRouteEpoch) {
		t.Fatalf("expected ErrStaleRouteEpoch for epoch 3 after Open(5), got %v", err)
	}
}

// ---- Reconcile --------------------------------------------------------------

func TestGate_Reconcile_ActiveThisCell_OpensGate(t *testing.T) {
	gr := newRegistry("cell-a")

	route := cells.TenantRoute{
		TenantID:   "t1",
		RouteEpoch: 3,
		ActiveCell: "cell-a",
		State:      cells.StateActive,
	}
	gr.Reconcile(route)

	tok, err := gr.TryEnter("t1", 3)
	if err != nil {
		t.Fatalf("expected gate open after Reconcile ACTIVE this cell: %v", err)
	}
	gr.Leave(tok)
}

func TestGate_Reconcile_MovingSourceThisCell_DrainsDeniesNew(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	route := cells.TenantRoute{
		TenantID:     "t1",
		RouteEpoch:   2,
		ActiveCell:   "cell-a",
		SourceCell:   "cell-a",
		TargetCell:   "cell-b",
		State:        cells.StateQuiescing,
		BarrierEpoch: 2,
	}
	gr.Reconcile(route)

	_, err := gr.TryEnter("t1", 1)
	if !errors.Is(err, cells.ErrTenantDraining) {
		t.Fatalf("expected ErrTenantDraining after Reconcile moving source, got %v", err)
	}
}

func TestGate_Reconcile_ActiveDifferentCell_ClosesGate(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t1", 1)

	// Tenant moved to cell-b.
	route := cells.TenantRoute{
		TenantID:   "t1",
		RouteEpoch: 2,
		ActiveCell: "cell-b",
		State:      cells.StateActive,
	}
	gr.Reconcile(route)

	_, err := gr.TryEnter("t1", 2)
	if !errors.Is(err, cells.ErrTenantDraining) {
		t.Fatalf("expected ErrTenantDraining after tenant moved to another cell, got %v", err)
	}
}

// ---- CellID -----------------------------------------------------------------

func TestGateRegistry_CellID(t *testing.T) {
	gr := cells.NewGateRegistry("my-cell", "inst-1")
	if gr.CellID() != "my-cell" {
		t.Errorf("expected CellID=my-cell, got %q", gr.CellID())
	}
}

func TestGateRegistry_EmptyCellID_DefaultsToDefault(t *testing.T) {
	gr := cells.NewGateRegistry("", "inst-1")
	if gr.CellID() != cells.DefaultCellID {
		t.Errorf("expected empty cellID to default to %q, got %q", cells.DefaultCellID, gr.CellID())
	}
}

// ---- Concurrent TryEnter ----------------------------------------------------

func TestGate_Concurrent_TryEnter_Leave(t *testing.T) {
	gr := newRegistry("cell-a")
	gr.Open("t-concurrent", 1)

	const goroutines = 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := gr.TryEnter("t-concurrent", 1)
			if err != nil {
				return
			}
			time.Sleep(time.Microsecond)
			gr.Leave(tok)
		}()
	}
	wg.Wait()
	if n := gr.Inflight("t-concurrent"); n != 0 {
		t.Errorf("expected inflight=0 after all goroutines done, got %d", n)
	}
}
