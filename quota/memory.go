package quota

import (
	"context"
	"sync"
	"time"
)

// MemoryMeter is an in-process [Meter] for development and tests. It keeps a
// per-(account, metric, window-bucket) committed counter guarded by a mutex.
// Reserve checks the live count against the [LimitSource] limit and optimistically
// holds the units (so concurrent reserves see them); Commit finalizes the hold,
// Release returns it.
//
// Limitation (dev approximation): for a STOCK metric (empty Window, e.g.
// "N sandboxes") the meter counts committed creates and never learns about
// deletes, so the count only grows. A production stock meter derives the live
// count from the resource store; the flow/rate case (a Window like "month") is
// exact within the window. Not for production.
type MemoryMeter struct {
	limits LimitSource
	now    func() time.Time

	mu   sync.Mutex
	used map[string]int64
}

// NewMemoryMeter returns a [MemoryMeter] reading limits from the given source.
func NewMemoryMeter(limits LimitSource) *MemoryMeter {
	return &MemoryMeter{limits: limits, now: time.Now, used: map[string]int64{}}
}

// Reserve implements [Meter].
func (m *MemoryMeter) Reserve(ctx context.Context, c Charge) (Reservation, error) {
	limit, has, err := m.limits.Limit(ctx, c.Account, c.Metric)
	if err != nil {
		return nil, err
	}
	if !has {
		return noopReservation{}, nil // no declared limit ⇒ unlimited
	}
	amt := c.Amount
	if amt <= 0 {
		amt = 1
	}
	key := c.Account + "|" + c.Metric + "|" + windowBucket(c.Window, m.now())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.used[key]+amt > limit {
		return nil, ErrOverLimit
	}
	m.used[key] += amt // hold optimistically so concurrent reserves see it
	return &memReservation{m: m, key: key, amount: amt}, nil
}

// windowBucket maps a rate window + instant to a stable bucket key. An empty
// window is a single (stock) bucket; an unknown window degrades to a single
// bucket named for the window rather than failing.
func windowBucket(window string, t time.Time) string {
	u := t.UTC()
	switch window {
	case "", "none":
		return ""
	case "minute":
		return u.Format("2006-01-02T15:04")
	case "hour":
		return u.Format("2006-01-02T15")
	case "day":
		return u.Format("2006-01-02")
	case "month":
		return u.Format("2006-01")
	default:
		return window
	}
}

type memReservation struct {
	m      *MemoryMeter
	key    string
	amount int64
	done   bool
}

// Commit finalizes the hold (the units stay counted).
func (r *memReservation) Commit(context.Context) error {
	r.done = true
	return nil
}

// Release returns the held units (only if not already finalized).
func (r *memReservation) Release(context.Context) error {
	if r.done {
		return nil
	}
	r.done = true
	r.m.mu.Lock()
	r.m.used[r.key] -= r.amount
	if r.m.used[r.key] <= 0 {
		delete(r.m.used, r.key)
	}
	r.m.mu.Unlock()
	return nil
}
