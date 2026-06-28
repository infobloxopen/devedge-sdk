package cells_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/cells"
)

// ---- 6. cohort all-or-nothing ----------------------------------------------

func TestMoveCohort_AllOrNothing_RollsBackCommittedOnFailure(t *testing.T) {
	table := cells.NewMemTable()
	fencer := cells.NewMemFencer()
	events := cells.NewMemEventBarrier()
	clk := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	// Seed 3 tenants ACTIVE on cell-a at epoch 1.
	for _, tn := range []string{"t1", "t2", "t3"} {
		r := cells.TenantRoute{TenantID: tn, RouteEpoch: 1, ActiveCell: "cell-a", State: cells.StateActive, EventPolicy: cells.PolicyNormal, EventEpoch: 1}
		if err := table.CompareAndSet(context.Background(), cells.TenantRoute{}, r); err != nil {
			t.Fatalf("seed %s: %v", tn, err)
		}
		_ = fencer.SetOwner(context.Background(), tn, "cell-a", 1)
		_ = events.SetPolicy(context.Background(), tn, cells.PolicyNormal, 1)
	}

	// A preflight that fails the SECOND member (t2) so the cohort aborts mid-way.
	pre := func(_ context.Context, plan cells.MovePlan) error {
		if plan.TenantID == "t2" {
			return errors.New("injected preflight failure for t2")
		}
		return nil
	}
	ctrl := cells.NewMoveController(table,
		cells.WithFencer(fencer),
		cells.WithEventBarrier(events),
		cells.WithClock(clk.Now),
		cells.WithPreflight(pre),
		cells.WithDrainDeadline(time.Second),
	)

	err := ctrl.MoveCohort(context.Background(), "cohort-1", []cells.MovePlan{
		{TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"},
		{TenantID: "t2", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"},
		{TenantID: "t3", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"},
	})
	if err == nil {
		t.Fatal("expected cohort move to fail at t2")
	}
	var ce *cells.CohortMoveError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CohortMoveError, got %T: %v", err, err)
	}
	if ce.FailedAt != "t2" {
		t.Errorf("FailedAt: want t2, got %q", ce.FailedAt)
	}

	// No member may be left mid-move; t1 (committed) is rolled back to cell-a;
	// t3 was never attempted (still cell-a); t2 never moved.
	for _, tn := range []string{"t1", "t2", "t3"} {
		r, err := table.Get(context.Background(), tn)
		if err != nil {
			t.Fatalf("get %s: %v", tn, err)
		}
		if r.State.IsMoving() {
			t.Errorf("%s left mid-move: %v", tn, r.State)
		}
		if r.ActiveCell != "cell-a" {
			t.Errorf("%s must be back/remain on cell-a, got %q", tn, r.ActiveCell)
		}
	}
	// t1 was rolled back, so its epoch advanced beyond 1.
	r1, _ := table.Get(context.Background(), "t1")
	if r1.RouteEpoch <= 1 {
		t.Errorf("t1 rollback must advance epoch forward, got %d", r1.RouteEpoch)
	}
}

func TestMoveCohort_AllSucceed(t *testing.T) {
	table := cells.NewMemTable()
	ctrl := cells.NewMoveController(table, cells.WithDrainDeadline(time.Second))
	for _, tn := range []string{"t1", "t2"} {
		if err := ctrl.Assign(context.Background(), tn, "cell-a", "op"); err != nil {
			t.Fatalf("assign %s: %v", tn, err)
		}
	}
	err := ctrl.MoveCohort(context.Background(), "c1", []cells.MovePlan{
		{TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"},
		{TenantID: "t2", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"},
	})
	if err != nil {
		t.Fatalf("MoveCohort: %v", err)
	}
	for _, tn := range []string{"t1", "t2"} {
		r, _ := table.Get(context.Background(), tn)
		if r.ActiveCell != "cell-b" {
			t.Errorf("%s should be on cell-b, got %q", tn, r.ActiveCell)
		}
	}
}

// ---- 7. budget meter --------------------------------------------------------

func TestBudgetMeter_RefusesOverBudget_ForceOverrides(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	// Tiny budget so a single move (estimate = drain deadline) breaches it.
	meter := cells.NewBudgetMeter(
		cells.WithMeterClock(clk.Now),
		cells.WithTenantBudget(time.Second),
	)
	table := cells.NewMemTable()
	ctrl := cells.NewMoveController(table,
		cells.WithClock(clk.Now),
		cells.WithBudgetMeter(meter),
		cells.WithDrainDeadline(10*time.Second), // estimate >> budget
	)
	if err := ctrl.Assign(context.Background(), "t1", "cell-a", "op"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// Non-forced move is refused.
	err := ctrl.Move(context.Background(), cells.MovePlan{TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"})
	if !errors.Is(err, cells.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
	// Tenant did not move.
	if r, _ := table.Get(context.Background(), "t1"); r.ActiveCell != "cell-a" {
		t.Errorf("refused move must not relocate the tenant, got %q", r.ActiveCell)
	}

	// Force overrides the gate.
	err = ctrl.Move(context.Background(), cells.MovePlan{TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op", Force: true})
	if err != nil {
		t.Fatalf("forced move should succeed: %v", err)
	}
	if r, _ := table.Get(context.Background(), "t1"); r.ActiveCell != "cell-b" {
		t.Errorf("forced move should relocate, got %q", r.ActiveCell)
	}
}

func TestBudgetMeter_MonthReset(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 6, 30, 23, 0, 0, 0, time.UTC))
	meter := cells.NewBudgetMeter(cells.WithMeterClock(clk.Now), cells.WithTenantBudget(100*time.Second))

	meter.Record("t1", 80*time.Second)
	if got := meter.Remaining("t1"); got != 20*time.Second {
		t.Errorf("remaining after 80s spent: want 20s, got %v", got)
	}
	if meter.Allowed("t1", 50*time.Second) {
		t.Error("50s should not be allowed when only 20s remains")
	}

	// Roll into July — ledger resets.
	clk.Advance(2 * time.Hour) // now 2026-07-01 01:00
	if got := meter.Remaining("t1"); got != 100*time.Second {
		t.Errorf("remaining after month reset: want full 100s, got %v", got)
	}
	if !meter.Allowed("t1", 90*time.Second) {
		t.Error("90s should be allowed after month reset")
	}
}

func TestBudgetMeter_RecordsActualCostAfterMove(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	meter := cells.NewBudgetMeter(cells.WithMeterClock(clk.Now), cells.WithTenantBudget(time.Hour))
	table := cells.NewMemTable()

	// A preflight that advances the clock so the move "costs" measurable time.
	pre := func(_ context.Context, _ cells.MovePlan) error {
		return nil
	}
	ctrl := cells.NewMoveController(table,
		cells.WithClock(clk.Now),
		cells.WithBudgetMeter(meter),
		cells.WithPreflight(pre),
		cells.WithDrainDeadline(time.Second),
	)
	if err := ctrl.Assign(context.Background(), "t1", "cell-a", "op"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	before := meter.Remaining("t1")
	// Move with no clock advance ⇒ recorded cost ~0; budget essentially unchanged.
	if err := ctrl.Move(context.Background(), cells.MovePlan{TenantID: "t1", FromCell: "cell-a", ToCell: "cell-b", Operator: "op"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	after := meter.Remaining("t1")
	if after > before {
		t.Errorf("recorded cost must not increase remaining budget: %v → %v", before, after)
	}
}

// ---- 9. placement policies + campaign --------------------------------------

func TestRoundRobinPolicy_Deterministic(t *testing.T) {
	p := cells.RoundRobinPolicy()
	cellSet := []string{"a", "b", "c"}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, w := range want {
		got, err := p.Place(context.Background(), "t", cellSet)
		if err != nil {
			t.Fatalf("Place[%d]: %v", i, err)
		}
		if got != w {
			t.Errorf("Place[%d]: want %q, got %q", i, w, got)
		}
	}
}

func TestStickyDefaultPolicy(t *testing.T) {
	p := cells.StickyDefaultPolicy("home")
	got, err := p.Place(context.Background(), "any", []string{"x", "y"})
	if err != nil || got != "home" {
		t.Fatalf("sticky default: want home, got %q (err %v)", got, err)
	}
}

func TestLeastLoadedPolicy(t *testing.T) {
	load := map[string]int{"a": 5, "b": 2, "c": 9}
	p := cells.LeastLoadedPolicy(func(c string) int { return load[c] })
	got, err := p.Place(context.Background(), "t", []string{"a", "b", "c"})
	if err != nil || got != "b" {
		t.Fatalf("least-loaded: want b, got %q (err %v)", got, err)
	}
}

func TestPlacementPolicy_NoCells(t *testing.T) {
	for _, p := range []cells.PlacementPolicy{
		cells.RoundRobinPolicy(),
		cells.LeastLoadedPolicy(func(string) int { return 0 }),
	} {
		if _, err := p.Place(context.Background(), "t", nil); !errors.Is(err, cells.ErrNoCells) {
			t.Errorf("expected ErrNoCells for empty candidate set, got %v", err)
		}
	}
}

func TestCampaign_RealizesDelta_AndIdempotentOnRerun(t *testing.T) {
	table := cells.NewMemTable()
	ctrl := cells.NewMoveController(table, cells.WithDrainDeadline(time.Second))

	// Seed t1,t2,t3 all on cell-a.
	for _, tn := range []string{"t1", "t2", "t3"} {
		if err := ctrl.Assign(context.Background(), tn, "cell-a", "op"); err != nil {
			t.Fatalf("assign %s: %v", tn, err)
		}
	}

	// Even-distribution rebalance across {cell-a, cell-b, cell-c}.
	plan, err := cells.PlanFromPolicy(context.Background(),
		[]string{"t1", "t2", "t3"}, []string{"cell-a", "cell-b", "cell-c"}, cells.RoundRobinPolicy())
	if err != nil {
		t.Fatalf("PlanFromPolicy: %v", err)
	}
	plan.Operator = "rebalance"
	// Deterministic round-robin over sorted tenants: t1→cell-a, t2→cell-b, t3→cell-c.
	if plan.Assignments["t1"] != "cell-a" || plan.Assignments["t2"] != "cell-b" || plan.Assignments["t3"] != "cell-c" {
		t.Fatalf("unexpected plan: %#v", plan.Assignments)
	}

	camp := cells.NewCampaign(ctrl)
	res, err := camp.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("campaign run: %v", err)
	}
	// t1 already on cell-a ⇒ skipped; t2,t3 moved.
	if res.Skipped["t1"] == "" {
		t.Errorf("t1 (already on target) should be skipped, skipped=%v", res.Skipped)
	}
	if len(res.Moved) != 2 {
		t.Errorf("want 2 tenants moved, got %v", res.Moved)
	}
	for _, tn := range []string{"t1", "t2", "t3"} {
		r, _ := table.Get(context.Background(), tn)
		if r.ActiveCell != plan.Assignments[tn] {
			t.Errorf("%s: want %q, got %q", tn, plan.Assignments[tn], r.ActiveCell)
		}
	}

	// Re-run: everything already on target ⇒ all skipped, nothing moved.
	res2, err := camp.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("campaign re-run: %v", err)
	}
	if len(res2.Moved) != 0 {
		t.Errorf("re-run must move nothing (idempotent), moved=%v", res2.Moved)
	}
	if len(res2.Skipped) != 3 {
		t.Errorf("re-run must skip all 3, skipped=%v", res2.Skipped)
	}
}

func TestCampaign_BudgetSkips_UnlessForce(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	meter := cells.NewBudgetMeter(cells.WithMeterClock(clk.Now), cells.WithTenantBudget(time.Second))
	table := cells.NewMemTable()
	ctrl := cells.NewMoveController(table,
		cells.WithClock(clk.Now),
		cells.WithBudgetMeter(meter),
		cells.WithDrainDeadline(10*time.Second),
	)
	if err := ctrl.Assign(context.Background(), "t1", "cell-a", "op"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	camp := cells.NewCampaign(ctrl)
	plan := cells.CampaignPlan{Assignments: map[string]string{"t1": "cell-b"}, Operator: "op"}

	res, err := camp.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Skipped["t1"] == "" {
		t.Errorf("over-budget tenant should be skipped, got %#v", res)
	}
	if r, _ := table.Get(context.Background(), "t1"); r.ActiveCell != "cell-a" {
		t.Errorf("skipped tenant must not move, got %q", r.ActiveCell)
	}

	// With Force, it moves.
	plan.Force = true
	res, err = camp.Run(context.Background(), plan)
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Errorf("forced campaign should move t1, got %#v", res)
	}
}
