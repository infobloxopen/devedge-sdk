package events

import "context"

// Leader is the pluggable leader-election / mutual-exclusion seam for the relay. Only
// ONE relay per service may pump the write-only outbox to the bus at a time; running two
// relays would double-publish every event (the bus and downstream idempotency dedup it,
// but it is wasteful and reorders nothing useful). The relay acquires leadership before
// each pump and yields it on stop, so exactly one replica drains the outbox.
//
// CLEAN CORE: this interface is backend-neutral — no broker, no driver, no ORM. The dev
// default is [SingleProcessLeader] (a pure-Go always-leader for the in-memory/embedded
// path, where there is only one process). A multi-replica SQL deployment plugs in a
// PostgreSQL advisory-lock / lease-row implementation OUTSIDE the core (it needs a
// driver, which the clean core forbids) — the relay only ever sees this interface.
//
// Contract: the relay calls TryAcquire before a pump; a true return means "you are the
// leader, proceed". A holder must keep the claim valid for the pump (an advisory-lock
// impl holds the session lock; a lease impl renews the lease). Release relinquishes it.
// A correct impl is at-most-one-leader: while one holder's claim is valid, every other
// TryAcquire returns false. At-least-once downstream tolerates a brief two-leader overlap
// during a failover (the idempotency marker dedups), so the seam need not be perfectly
// fenced — it only has to keep the steady state to a single active relay.
type Leader interface {
	// TryAcquire attempts to become (or remain) the leader. It returns true if this
	// caller now holds leadership, false if another holder does. It must be safe to
	// call repeatedly by the current holder (a re-acquire by the holder returns true).
	TryAcquire(ctx context.Context) (acquired bool, err error)

	// Release relinquishes leadership held by this caller, so another replica can
	// acquire it. It is safe to call when not currently held (a no-op).
	Release(ctx context.Context) error
}

// SingleProcessLeader is the dev/embedded [Leader]: it grants leadership to whichever
// in-process caller holds it and denies it to any other concurrent caller — process-local
// mutual exclusion, no coordination across processes. It is the correct default for the
// in-memory bus path (single process, single relay) and for any single-replica
// deployment. A multi-replica deployment must plug in a cross-process Leader (PG advisory
// lock / lease) instead.
//
// It is genuinely a lock, not an unconditional yes: two relays constructed against the
// SAME SingleProcessLeader instance still get single-leader behaviour in one process
// (the second TryAcquire returns false until the first Release), which is what lets a
// test assert "only one relay is active" without a real database.
type SingleProcessLeader struct {
	held chan struct{} // a buffered(1) channel used as a try-lock; full == held
}

// NewSingleProcessLeader returns an unheld in-process leader lock.
func NewSingleProcessLeader() *SingleProcessLeader {
	return &SingleProcessLeader{held: make(chan struct{}, 1)}
}

// TryAcquire implements [Leader]: grab the in-process lock if free. A holder that calls
// it again does NOT re-grab (the lock is already full) — but the relay always pairs an
// acquire with a Release, and a holder re-acquiring would see false; to keep the holder's
// re-acquire returning true the relay holds leadership across a whole Run loop rather than
// per-pump for this impl. See [Relay] for how it is used.
func (l *SingleProcessLeader) TryAcquire(ctx context.Context) (bool, error) {
	select {
	case l.held <- struct{}{}:
		return true, nil
	default:
		return false, nil
	}
}

// Release implements [Leader]: free the in-process lock (a no-op if not held).
func (l *SingleProcessLeader) Release(ctx context.Context) error {
	select {
	case <-l.held:
	default:
	}
	return nil
}

// compile-time check.
var _ Leader = (*SingleProcessLeader)(nil)
