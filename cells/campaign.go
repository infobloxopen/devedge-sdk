package cells

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// CampaignPlan is a desired tenant→cell placement to realize across many tenants
// (e.g. an even-distribution rebalance). Only tenants whose current cell differs
// from their target are moved; the rest are skipped, so a campaign is idempotent
// and resumable — re-running converges without re-moving placed tenants.
type CampaignPlan struct {
	Assignments   map[string]string // tenant → desired cell
	MaxConcurrent int               // bound on simultaneous moves (<=1 ⇒ sequential)
	Operator      string
	Force         bool // bypass the budget gate for every member
}

// CampaignResult records the per-tenant outcome of a [Campaign.Run].
type CampaignResult struct {
	Moved   []string          // tenants successfully moved to their target
	Skipped map[string]string // tenant → reason (already placed, budget, ctx cancelled)
	Failed  map[string]error  // tenant → move error
}

// CampaignResult skip reasons.
const (
	skipAlreadyPlaced = "already on target cell"
	skipBudget        = "would exceed unavailability budget"
	skipCancelled     = "campaign cancelled before move"
)

// Campaign realizes a [CampaignPlan] as a sequence (or bounded-concurrency set)
// of moves driven by a [MoveController]. It is abortable via ctx, budget-aware
// (a tenant whose move would breach budget is skipped unless Force is set), and
// idempotent on re-run.
type Campaign struct {
	ctrl   *MoveController
	budget *BudgetMeter // mirrors the controller's meter so we can pre-skip
}

// NewCampaign builds a campaign over ctrl. If ctrl has a budget meter the campaign
// uses it to pre-skip over-budget tenants (the controller still enforces it).
func NewCampaign(ctrl *MoveController) *Campaign {
	return &Campaign{ctrl: ctrl, budget: ctrl.budget}
}

// Run realizes plan. For each tenant whose current cell differs from its target it
// drives a move; tenants already on target are skipped, budget-blocked tenants are
// skipped (unless plan.Force), and a cancelled ctx stops launching further moves.
// The result tallies moved/skipped/failed; Run returns a non-nil error only when
// at least one move failed (the per-tenant causes are in CampaignResult.Failed).
func (c *Campaign) Run(ctx context.Context, plan CampaignPlan) (CampaignResult, error) {
	res := CampaignResult{
		Skipped: make(map[string]string),
		Failed:  make(map[string]error),
	}

	// Deterministic order so a campaign is reproducible and resumable.
	tenants := make([]string, 0, len(plan.Assignments))
	for t := range plan.Assignments {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)

	conc := plan.MaxConcurrent
	if conc < 1 {
		conc = 1
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, conc)
	)

	for _, tenant := range tenants {
		target := plan.Assignments[tenant]

		// Resolve the current placement to decide skip-vs-move (idempotency).
		cur, err := c.ctrl.table.Get(ctx, tenant)
		hasRoute := false
		switch {
		case errors.Is(err, ErrNoRoute):
			// No route yet — Assign creates it; if the target is the only cell, this
			// is a first placement, not a move.
		case err != nil:
			mu.Lock()
			res.Failed[tenant] = err
			mu.Unlock()
			continue
		default:
			hasRoute = true
			if cur.State.AdmitsNew() && cur.ActiveCell == target {
				mu.Lock()
				res.Skipped[tenant] = skipAlreadyPlaced
				mu.Unlock()
				continue
			}
		}

		if ctx.Err() != nil {
			mu.Lock()
			res.Skipped[tenant] = skipCancelled
			mu.Unlock()
			continue
		}

		// Budget pre-skip (the controller is still the enforcer). First placement
		// (no route) is not a move and never debits the budget.
		if hasRoute && c.budget != nil && !plan.Force && !c.budget.Allowed(tenant, c.ctrl.estimateCost()) {
			mu.Lock()
			res.Skipped[tenant] = skipBudget
			mu.Unlock()
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(tenant, target, fromCell string, hasRoute bool) {
			defer wg.Done()
			defer func() { <-sem }()

			var err error
			if hasRoute {
				// Relocate an existing tenant; carry Force so the controller's own
				// budget gate honors the campaign's Force decision.
				err = c.ctrl.Move(ctx, MovePlan{
					TenantID: tenant, FromCell: fromCell, ToCell: target,
					Operator: plan.Operator, Force: plan.Force,
				})
			} else {
				err = c.ctrl.Assign(ctx, tenant, target, plan.Operator)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				res.Moved = append(res.Moved, tenant)
			case errors.Is(err, ErrBudgetExceeded):
				res.Skipped[tenant] = skipBudget
			default:
				res.Failed[tenant] = err
			}
		}(tenant, target, cur.ActiveCell, hasRoute)
	}
	wg.Wait()

	sort.Strings(res.Moved)
	if len(res.Failed) > 0 {
		return res, errors.New("cells: campaign completed with failures")
	}
	return res, nil
}

// PlanFromPolicy builds a [CampaignPlan] by asking policy to place each tenant
// across cells — the even-distribution rebalance helper. The plan is sequential
// (MaxConcurrent 1) by default; callers raise it. Placement is computed in sorted
// tenant order so a deterministic policy (e.g. round-robin) yields a stable plan.
func PlanFromPolicy(ctx context.Context, tenants []string, cells []string, policy PlacementPolicy) (CampaignPlan, error) {
	plan := CampaignPlan{
		Assignments:   make(map[string]string, len(tenants)),
		MaxConcurrent: 1,
	}
	ordered := append([]string(nil), tenants...)
	sort.Strings(ordered)
	for _, t := range ordered {
		cell, err := policy.Place(ctx, t, cells)
		if err != nil {
			return CampaignPlan{}, err
		}
		plan.Assignments[t] = cell
	}
	return plan, nil
}
