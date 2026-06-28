package cells

import (
	"errors"
	"sync"
	"time"
)

// ErrBudgetExceeded is returned when a move would spend more of a tenant's
// monthly unavailability budget than remains — a non-forced move is refused
// rather than risk breaching the availability target.
var ErrBudgetExceeded = errors.New("cells: move would exceed tenant unavailability budget")

// DefaultMonthlyBudget is the default per-tenant unavailability allowance for one
// calendar month. 130s/month is roughly a 99.995% monthly availability target —
// the headroom a drain-and-cutover move spends on the brief reject window.
const DefaultMonthlyBudget = 130 * time.Second

// BudgetMeter is a per-tenant monthly unavailability ledger. The move controller
// debits a tenant's budget by the unavailability a move costs (the reject window)
// and refuses a non-forced move whose estimate would breach the remainder. The
// ledger resets at each calendar-month boundary, observed through an injected
// clock so tests are deterministic.
//
// It is concurrency-safe: a campaign may consult and record many tenants at once.
type BudgetMeter struct {
	now           func() time.Time
	defaultBudget time.Duration

	mu      sync.Mutex
	ledgers map[string]*budgetLedger
}

type budgetLedger struct {
	year  int
	month time.Month
	spent time.Duration
}

// BudgetOption configures a [BudgetMeter].
type BudgetOption func(*BudgetMeter)

// WithMeterClock injects the clock the meter reads for month boundaries (default
// [time.Now]). Tests pass a controllable clock so resets are deterministic.
func WithMeterClock(now func() time.Time) BudgetOption {
	return func(m *BudgetMeter) {
		if now != nil {
			m.now = now
		}
	}
}

// WithTenantBudget overrides the per-tenant monthly budget (default
// [DefaultMonthlyBudget]).
func WithTenantBudget(d time.Duration) BudgetOption {
	return func(m *BudgetMeter) {
		if d > 0 {
			m.defaultBudget = d
		}
	}
}

// NewBudgetMeter returns a meter with the default budget and clock unless
// overridden by options.
func NewBudgetMeter(opts ...BudgetOption) *BudgetMeter {
	m := &BudgetMeter{
		now:           time.Now,
		defaultBudget: DefaultMonthlyBudget,
		ledgers:       make(map[string]*budgetLedger),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// ledgerLocked returns the tenant's ledger for the current month, resetting it if
// the calendar month has rolled over. Caller must hold m.mu.
func (m *BudgetMeter) ledgerLocked(tenantID string) *budgetLedger {
	t := m.now()
	y, mon := t.Year(), t.Month()
	l, ok := m.ledgers[tenantID]
	if !ok {
		l = &budgetLedger{year: y, month: mon}
		m.ledgers[tenantID] = l
		return l
	}
	if l.year != y || l.month != mon {
		l.year, l.month, l.spent = y, mon, 0
	}
	return l
}

// Remaining reports how much unavailability budget tenantID has left this month.
// It never returns negative.
func (m *BudgetMeter) Remaining(tenantID string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.ledgerLocked(tenantID)
	r := m.defaultBudget - l.spent
	if r < 0 {
		return 0
	}
	return r
}

// Allowed reports whether spending estimate would stay within tenantID's
// remaining budget this month.
func (m *BudgetMeter) Allowed(tenantID string, estimate time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.ledgerLocked(tenantID)
	return l.spent+estimate <= m.defaultBudget
}

// Record debits tenantID's budget by spent (the unavailability a completed move
// actually cost). Negative spends are ignored.
func (m *BudgetMeter) Record(tenantID string, spent time.Duration) {
	if spent <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.ledgerLocked(tenantID)
	l.spent += spent
}
