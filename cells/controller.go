package cells

import "context"

// GateController keeps a [GateRegistry]'s gates aligned with the routing table by
// consuming its watch stream and calling [GateRegistry.Reconcile] for each change.
// This is how a cell enforces the *current* tenant epoch: a tenant that moves away
// has its gate closed here, so a stale upstream router cannot get work admitted.
//
// It reconciles forward from the watch (no bulk enumeration): tenants not yet seen
// are admitted via TryEnter's lazy open at their resolved epoch, and corrected the
// moment a route change for them arrives.
type GateController struct {
	reg         *GateRegistry
	table       RoutingTable
	defaultCell string
}

// NewGateController binds a registry to a table. defaultCell names the fail-safe
// cell so a deleted route reverts correctly: the default cell reopens the tenant,
// any other cell closes it.
func NewGateController(reg *GateRegistry, table RoutingTable, defaultCell string) *GateController {
	if defaultCell == "" {
		defaultCell = DefaultCellID
	}
	return &GateController{reg: reg, table: table, defaultCell: defaultCell}
}

// Start begins watching and reconciling until ctx is cancelled. It returns once
// the watch is established; the background loop reconciles thereafter.
func (c *GateController) Start(ctx context.Context) error {
	ch, err := c.table.Watch(ctx)
	if err != nil {
		return err
	}
	go c.loop(ctx, ch)
	return nil
}

func (c *GateController) loop(ctx context.Context, ch <-chan RouteEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Deleted {
				if c.reg.cellID == c.defaultCell {
					// Tenant reverts to the default cell: forget the local gate so the
					// next call lazily re-admits at epoch 0 (Reset, not Open, because a
					// removed route has no epoch and Open is monotonic).
					c.reg.Reset(ev.TenantID)
				} else {
					c.reg.closeGate(ev.TenantID)
				}
				continue
			}
			c.reg.Reconcile(ev.Route)
		}
	}
}
