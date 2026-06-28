package cells_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/cells"
)

// ---- harness ----------------------------------------------------------------

// moveHarness wires a full in-memory move stack: table + source/target gate
// registries + a GateDrainer on the source + fencer + event barrier, plus a
// MoveController over them with a deterministic clock and id generator.
type moveHarness struct {
	table    *cells.MemTable
	srcReg   *cells.GateRegistry
	dstReg   *cells.GateRegistry
	fencer   *cells.MemFencer
	events   *cells.MemEventBarrier
	ctrl     *cells.MoveController
	clock    *fakeClock
	idGen    func() string
	fromCell string
	toCell   string
}

// fakeClock is a controllable monotonic clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newMoveHarness(t *testing.T, from, to string, opts ...cells.ControllerOption) *moveHarness {
	t.Helper()
	clk := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	var idMu sync.Mutex
	var idN int
	idGen := func() string {
		idMu.Lock()
		defer idMu.Unlock()
		idN++
		return "b" + strconv.Itoa(idN)
	}
	h := &moveHarness{
		table:    cells.NewMemTable(),
		srcReg:   cells.NewGateRegistry(from, "inst-src"),
		dstReg:   cells.NewGateRegistry(to, "inst-dst"),
		fencer:   cells.NewMemFencer(),
		events:   cells.NewMemEventBarrier(),
		clock:    clk,
		idGen:    idGen,
		fromCell: from,
		toCell:   to,
	}
	base := []cells.ControllerOption{
		cells.WithFencer(h.fencer),
		cells.WithEventBarrier(h.events),
		cells.WithDrainer(cells.NewGateDrainer(h.srcReg)),
		cells.WithClock(clk.Now),
		cells.WithIDGenerator(idGen),
		cells.WithDrainDeadline(2 * time.Second),
	}
	h.ctrl = cells.NewMoveController(h.table, append(base, opts...)...)
	return h
}

// seed creates an ACTIVE route at the given epoch on the source cell and opens
// the source gate there.
func (h *moveHarness) seed(t *testing.T, tenant string, epoch uint64) {
	t.Helper()
	r := cells.TenantRoute{
		TenantID:    tenant,
		RouteEpoch:  epoch,
		ActiveCell:  h.fromCell,
		State:       cells.StateActive,
		EventPolicy: cells.PolicyNormal,
		EventEpoch:  epoch,
	}
	if err := h.table.CompareAndSet(context.Background(), cells.TenantRoute{}, r); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.srcReg.Open(tenant, epoch)
	if err := h.fencer.SetOwner(context.Background(), tenant, h.fromCell, epoch); err != nil {
		t.Fatalf("seed fence: %v", err)
	}
	if err := h.events.SetPolicy(context.Background(), tenant, cells.PolicyNormal, epoch); err != nil {
		t.Fatalf("seed events: %v", err)
	}
}

func (h *moveHarness) get(t *testing.T, tenant string) cells.TenantRoute {
	t.Helper()
	r, err := h.table.Get(context.Background(), tenant)
	if err != nil {
		t.Fatalf("get %q: %v", tenant, err)
	}
	return r
}

// ---- 1. happy path ----------------------------------------------------------

func TestMove_HappyPath_AToB(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	h.seed(t, "t1", 5)
	// Reconcile the target gate so it opens on commit (simulating its watch).
	go reconcileLoop(t, h, "t1")

	if err := h.ctrl.Move(context.Background(), cells.MovePlan{
		TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op",
	}); err != nil {
		t.Fatalf("Move: %v", err)
	}

	r := h.get(t, "t1")
	if r.RouteEpoch != 7 {
		t.Errorf("epoch: want 7 (N+2 from 5), got %d", r.RouteEpoch)
	}
	if r.ActiveCell != "cell-b" {
		t.Errorf("ActiveCell: want cell-b, got %q", r.ActiveCell)
	}
	if r.State != cells.StateActive {
		t.Errorf("State: want ACTIVE, got %v", r.State)
	}
	if r.SourceCell != "" || r.TargetCell != "" || r.BarrierID != "" {
		t.Errorf("move fields not cleared: %+v", r)
	}
	if r.EventPolicy != cells.PolicyNormal || r.EventEpoch != 7 {
		t.Errorf("event policy/epoch: want NORMAL@7, got %v@%d", r.EventPolicy, r.EventEpoch)
	}
	// Fencer: owner is target at epoch 7; the old cell at the old epoch is fenced.
	if !h.fencer.Allow("t1", "cell-b", 7) {
		t.Error("target should be allowed to write at epoch 7")
	}
	if h.fencer.Allow("t1", "cell-a", 5) {
		t.Error("old cell at old epoch must be fenced after commit")
	}
	// Event barrier NORMAL at the post-move epoch.
	if pol, ep := h.events.Policy("t1"); pol != cells.PolicyNormal || ep != 7 {
		t.Errorf("event barrier: want NORMAL@7, got %v@%d", pol, ep)
	}
}

// reconcileLoop watches the table and reconciles both registries — simulating the
// GateController on each cell so the target gate opens on commit.
func reconcileLoop(t *testing.T, h *moveHarness, _ string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := h.table.Watch(ctx)
	if err != nil {
		return
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			h.srcReg.Reconcile(ev.Route)
			h.dstReg.Reconcile(ev.Route)
		}
	}
}

// ---- 2. concurrent writer at old epoch is fenced ----------------------------

func TestMove_ConcurrentOldEpochWriter_Fenced(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	h.seed(t, "t1", 1)

	// Before the move, the source at epoch 1 may write.
	if !h.fencer.Allow("t1", "cell-a", 1) {
		t.Fatal("pre-move: source should be allowed")
	}

	if err := h.ctrl.Move(context.Background(), cells.MovePlan{
		TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op",
	}); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// A zombie writer from the old cell at the old epoch is now fenced.
	if h.fencer.Allow("t1", "cell-a", 1) {
		t.Error("stale writer (cell-a@1) must be fenced after the move commits")
	}
	// And the source gate no longer admits at the old epoch.
	if _, err := h.srcReg.TryEnter("t1", 1); err == nil {
		t.Error("source gate should not admit at the old epoch after the move")
	}
}

// ---- 3. rollback before commit ----------------------------------------------

func TestMove_Rollback_SourceStaysActive_EpochForward(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	h.seed(t, "t1", 4)

	// Force a failure during the move: the fencer rejects Seal so drive() fails at
	// Phase 3, triggering rollback. We use a controller whose fencer fails Seal.
	failFencer := &sealFailFencer{MemFencer: h.fencer}
	ctrl := cells.NewMoveController(h.table,
		cells.WithFencer(failFencer),
		cells.WithEventBarrier(h.events),
		cells.WithDrainer(cells.NewGateDrainer(h.srcReg)),
		cells.WithClock(h.clock.Now),
		cells.WithIDGenerator(h.idGen),
		cells.WithDrainDeadline(2*time.Second),
	)

	err := ctrl.Move(context.Background(), cells.MovePlan{
		TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op",
	})
	if err == nil {
		t.Fatal("expected Move to fail (Seal rejected)")
	}

	r := h.get(t, "t1")
	if r.ActiveCell != "cell-a" {
		t.Errorf("after rollback ActiveCell must stay cell-a, got %q", r.ActiveCell)
	}
	if !r.State.AdmitsNew() {
		t.Errorf("after rollback state must admit new work, got %v", r.State)
	}
	if r.RouteEpoch <= 4 {
		t.Errorf("epoch must advance forward on rollback (was 4), got %d", r.RouteEpoch)
	}
	if r.EventPolicy != cells.PolicyNormal {
		t.Errorf("event policy must be NORMAL after rollback, got %v", r.EventPolicy)
	}
	// Owner is the source at the new (higher) epoch.
	if !h.fencer.Allow("t1", "cell-a", r.RouteEpoch) {
		t.Error("source should own the tenant at the new epoch after rollback")
	}
}

// sealFailFencer wraps a MemFencer but always fails Seal, to force a mid-move
// failure for rollback tests.
type sealFailFencer struct{ *cells.MemFencer }

func (f *sealFailFencer) Seal(ctx context.Context, tenantID string, barrierEpoch uint64) error {
	return errors.New("injected seal failure")
}

// ---- 4. crash-resume from each move state -----------------------------------

func TestResume_FromEachMoveState(t *testing.T) {
	states := []struct {
		name  string
		state cells.State
	}{
		{"quiescing", cells.StateQuiescing},
		{"draining", cells.StateDraining},
		{"copying", cells.StateCopying},
		{"committing", cells.StateCommitting},
	}
	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			h := newMoveHarness(t, "cell-a", "cell-b")
			// Persist a route mid-move (as a crash would leave it). Epoch N=2,
			// barrier N+1=3, commit N+2=4. Deadline far in the future.
			mid := cells.TenantRoute{
				TenantID:     "t1",
				RouteEpoch:   2,
				ActiveCell:   "cell-a",
				SourceCell:   "cell-a",
				TargetCell:   "cell-b",
				State:        tc.state,
				BarrierEpoch: 3,
				BarrierID:    "b1",
				EventPolicy:  cells.PolicyPause,
				EventEpoch:   3,
				Deadline:     h.clock.Now().Add(time.Hour),
			}
			if err := h.table.CompareAndSet(context.Background(), cells.TenantRoute{}, mid); err != nil {
				t.Fatalf("seed mid-move: %v", err)
			}
			// Seal the fencer to match a real mid-move barrier.
			_ = h.fencer.Seal(context.Background(), "t1", 3)

			if err := h.ctrl.Resume(context.Background(), "t1"); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			r := h.get(t, "t1")
			if r.State != cells.StateActive {
				t.Fatalf("Resume must reach ACTIVE, got %v", r.State)
			}
			if r.ActiveCell != "cell-b" {
				t.Errorf("Resume must complete the move to cell-b, got %q", r.ActiveCell)
			}
			if r.RouteEpoch != 4 {
				t.Errorf("Resume must commit at epoch 4 (N+2), got %d", r.RouteEpoch)
			}
			if !h.fencer.Allow("t1", "cell-b", 4) {
				t.Error("target should own at epoch 4 after resumed commit")
			}
		})
	}
}

func TestResume_PastDeadline_RollsBack(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	mid := cells.TenantRoute{
		TenantID:     "t1",
		RouteEpoch:   2,
		ActiveCell:   "cell-a",
		SourceCell:   "cell-a",
		TargetCell:   "cell-b",
		State:        cells.StateDraining,
		BarrierEpoch: 3,
		Deadline:     h.clock.Now().Add(-time.Minute), // already elapsed
	}
	if err := h.table.CompareAndSet(context.Background(), cells.TenantRoute{}, mid); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.ctrl.Resume(context.Background(), "t1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	r := h.get(t, "t1")
	if r.ActiveCell != "cell-a" || r.State != cells.StateActive {
		t.Errorf("past-deadline resume must roll back to source ACTIVE, got %q/%v", r.ActiveCell, r.State)
	}
	if r.RouteEpoch <= 2 {
		t.Errorf("rollback epoch must advance forward, got %d", r.RouteEpoch)
	}
}

func TestResume_Committing_FinishesForward_EvenPastDeadline(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	mid := cells.TenantRoute{
		TenantID:     "t1",
		RouteEpoch:   2,
		ActiveCell:   "cell-a",
		SourceCell:   "cell-a",
		TargetCell:   "cell-b",
		State:        cells.StateCommitting,
		BarrierEpoch: 3,
		EventPolicy:  cells.PolicyPause,
		EventEpoch:   3,
		Deadline:     h.clock.Now().Add(-time.Minute), // elapsed, but COMMITTING is past PONR
	}
	if err := h.table.CompareAndSet(context.Background(), cells.TenantRoute{}, mid); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.ctrl.Resume(context.Background(), "t1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	r := h.get(t, "t1")
	if r.ActiveCell != "cell-b" || r.State != cells.StateActive || r.RouteEpoch != 4 {
		t.Errorf("COMMITTING must finish forward to cell-b@4 ACTIVE, got %q/%v@%d", r.ActiveCell, r.State, r.RouteEpoch)
	}
}

// ---- 5. idempotency ---------------------------------------------------------

func TestMove_Idempotent_DoubleMove(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	h.seed(t, "t1", 1)

	plan := cells.MovePlan{TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"}
	if err := h.ctrl.Move(context.Background(), plan); err != nil {
		t.Fatalf("first Move: %v", err)
	}
	first := h.get(t, "t1")

	// Second Move to the same target is a no-op (already resting there).
	if err := h.ctrl.Move(context.Background(), plan); err != nil {
		t.Fatalf("second Move: %v", err)
	}
	second := h.get(t, "t1")
	if second.RouteEpoch != first.RouteEpoch {
		t.Errorf("epoch must not advance on a no-op re-Move: %d → %d", first.RouteEpoch, second.RouteEpoch)
	}
}

func TestResume_Idempotent_DoubleResume(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	h.seed(t, "t1", 1)
	if err := h.ctrl.Move(context.Background(), cells.MovePlan{
		TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op",
	}); err != nil {
		t.Fatalf("Move: %v", err)
	}
	before := h.get(t, "t1")
	for i := 0; i < 3; i++ {
		if err := h.ctrl.Resume(context.Background(), "t1"); err != nil {
			t.Fatalf("Resume[%d]: %v", i, err)
		}
	}
	after := h.get(t, "t1")
	if after.RouteEpoch != before.RouteEpoch {
		t.Errorf("Resume of a resting route must not advance epoch: %d → %d", before.RouteEpoch, after.RouteEpoch)
	}
}

// ---- 8. Assign --------------------------------------------------------------

func TestAssign_FirstPlacement_CreatesEpoch1(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	if err := h.ctrl.Assign(context.Background(), "t1", "cell-a", "op"); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	r := h.get(t, "t1")
	if r.RouteEpoch != 1 || r.ActiveCell != "cell-a" || r.State != cells.StateActive {
		t.Errorf("first placement: want cell-a@1 ACTIVE, got %q@%d/%v", r.ActiveCell, r.RouteEpoch, r.State)
	}
}

func TestAssign_SameCell_NoOp(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	if err := h.ctrl.Assign(context.Background(), "t1", "cell-a", "op"); err != nil {
		t.Fatalf("Assign 1: %v", err)
	}
	first := h.get(t, "t1")
	if err := h.ctrl.Assign(context.Background(), "t1", "cell-a", "op"); err != nil {
		t.Fatalf("Assign 2: %v", err)
	}
	second := h.get(t, "t1")
	if second.RouteEpoch != first.RouteEpoch {
		t.Errorf("re-Assign to same cell must be a no-op: %d → %d", first.RouteEpoch, second.RouteEpoch)
	}
}

func TestAssign_DifferentCell_Moves(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	if err := h.ctrl.Assign(context.Background(), "t1", "cell-a", "op"); err != nil {
		t.Fatalf("Assign 1: %v", err)
	}
	if err := h.ctrl.Assign(context.Background(), "t1", "cell-b", "op"); err != nil {
		t.Fatalf("Assign 2 (move): %v", err)
	}
	r := h.get(t, "t1")
	if r.ActiveCell != "cell-b" {
		t.Errorf("Assign to a different cell must move: want cell-b, got %q", r.ActiveCell)
	}
	if r.RouteEpoch != 3 { // 1 → move → 3
		t.Errorf("epoch after first-placement(1)+move: want 3, got %d", r.RouteEpoch)
	}
}

// ---- invariant: epoch never decreases across a full lifecycle ---------------

func TestInvariant_EpochMonotonicAcrossLifecycle(t *testing.T) {
	h := newMoveHarness(t, "cell-a", "cell-b")
	go reconcileLoop(t, h, "t1")

	// Record every epoch the watch observes; assert non-decreasing.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := h.table.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	var mu sync.Mutex
	var observed []uint64
	go func() {
		for ev := range ch {
			mu.Lock()
			observed = append(observed, ev.Route.RouteEpoch)
			mu.Unlock()
		}
	}()

	if err := h.ctrl.Assign(context.Background(), "t1", "cell-a", "op"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := h.ctrl.Move(context.Background(), cells.MovePlan{TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	// And a rollback-inducing move back that fails.
	failFencer := &sealFailFencer{MemFencer: h.fencer}
	ctrl2 := cells.NewMoveController(h.table,
		cells.WithFencer(failFencer),
		cells.WithDrainer(cells.NewGateDrainer(h.dstReg)),
		cells.WithClock(h.clock.Now),
		cells.WithIDGenerator(h.idGen),
		cells.WithDrainDeadline(time.Second),
	)
	_ = ctrl2.Move(context.Background(), cells.MovePlan{TenantID: "t1", FromCell: "cell-b", ToCell: "cell-a", Operator: "op"})

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	var prev uint64
	for i, e := range observed {
		if e < prev {
			t.Errorf("epoch regressed at observation %d: %d after %d", i, e, prev)
		}
		prev = e
	}
	if len(observed) == 0 {
		t.Fatal("expected to observe route events")
	}
}
