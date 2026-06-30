// Package quota is the usage/quota metering seam (P13) — deliberately SEPARATE
// from the authz decision (P12). A boolean PDP decision cannot both pre-check a
// limit (count < limit) and consume-only-on-success; quota needs a
// reserve→commit/release lifecycle keyed by (account, metric, window). So the
// gate decides allow/deny, and — for a method that declares a quota — this seam
// meters it AROUND the handler.
//
// This matches the Infoblox reality: the OPA sidecar evaluates boolean features,
// while a usage service (nios.token-allocation-service) meters quotas and lives
// apart. The dev default ([MemoryMeter] + [StaticLimits]) keeps a service
// runnable locally; the enterprise binding is the token-allocation/usage
// service. Limits flow from the entitlement data (the licensed quantities for an
// account); usage is the meter's own state.
package quota

import (
	"context"
	"errors"
	"maps"
)

// ErrOverLimit is returned by [Meter.Reserve] when the charge would exceed the
// account's limit for the metric. Bindings map it to codes.ResourceExhausted.
var ErrOverLimit = errors.New("quota: over limit")

// Charge is one metered consumption: Amount units of Metric, billed to Account,
// within the rate Window (empty Window = a stock/count, no window).
type Charge struct {
	Account string
	Metric  string
	Window  string
	Amount  int64
}

// Meter checks a [Charge] against the account's limit and tentatively reserves
// it. The returned [Reservation] is finalized with Commit (the operation
// succeeded) or undone with Release (it failed) — so a failed operation
// consumes nothing.
type Meter interface {
	Reserve(ctx context.Context, c Charge) (Reservation, error)
}

// Reservation is the outcome of a successful [Meter.Reserve]. Exactly one of
// Commit / Release should be called.
type Reservation interface {
	// Commit confirms the consumption (the operation succeeded).
	Commit(ctx context.Context) error
	// Release returns the reserved units (the operation failed / rolled back).
	Release(ctx context.Context) error
}

// LimitSource reports the limit for (account, metric). Limits come from the
// entitlement data (the licensed quantities for an account); the dev default is
// [StaticLimits]. It composes with the per-tenant rules substrate (rules.Source)
// the same way the feature source and tag rules do.
type LimitSource interface {
	// Limit returns the account's limit for the metric and whether one is
	// declared. has=false means "no declared limit" — treated as unlimited.
	Limit(ctx context.Context, account, metric string) (limit int64, has bool, err error)
}

// LimitSourceFunc adapts a function to a [LimitSource].
type LimitSourceFunc func(ctx context.Context, account, metric string) (int64, bool, error)

// Limit implements [LimitSource].
func (f LimitSourceFunc) Limit(ctx context.Context, account, metric string) (int64, bool, error) {
	return f(ctx, account, metric)
}

// StaticLimits is an in-memory [LimitSource] for development and tests: a fixed
// per-account, per-metric limit table. Not for production — the enterprise
// binding is the entitlement/usage service.
type StaticLimits struct {
	byAccount map[string]map[string]int64
}

// NewStaticLimits builds a [StaticLimits] from account → metric → limit.
func NewStaticLimits(byAccount map[string]map[string]int64) *StaticLimits {
	cp := make(map[string]map[string]int64, len(byAccount))
	for acct, metrics := range byAccount {
		m := make(map[string]int64, len(metrics))
		maps.Copy(m, metrics)
		cp[acct] = m
	}
	return &StaticLimits{byAccount: cp}
}

// Limit implements [LimitSource].
func (s *StaticLimits) Limit(_ context.Context, account, metric string) (int64, bool, error) {
	if metrics, ok := s.byAccount[account]; ok {
		if lim, ok := metrics[metric]; ok {
			return lim, true, nil
		}
	}
	return 0, false, nil
}

// noopReservation is returned when no limit is declared (unlimited): Commit and
// Release are no-ops.
type noopReservation struct{}

func (noopReservation) Commit(context.Context) error  { return nil }
func (noopReservation) Release(context.Context) error { return nil }
