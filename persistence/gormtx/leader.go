// leader.go — the PostgreSQL advisory-lock implementation of the events.Leader seam
// (Phase 2 of the SDK event-bus stack). It is the cross-process leader the relay plugs
// in for a MULTI-REPLICA deployment: of N service replicas each running a relay, exactly
// ONE holds the advisory lock and pumps the outbox→Kafka, so the broker is not flooded
// with N copies of every event. The in-memory events.SingleProcessLeader stays the dev
// default (one process, one relay); this is its production twin.
//
// Mechanism — pg_advisory_lock (session-scoped):
//
//	A PostgreSQL session-level advisory lock (pg_try_advisory_lock(key)) is held by ONE
//	session at a time across the whole database, cluster-wide — exactly the
//	at-most-one-leader the events.Leader contract wants. We hold the lock on a DEDICATED,
//	single connection pinned for the lock's lifetime (a *sql.Conn with MaxOpenConns=1),
//	because a session advisory lock is released only by the SAME session that took it
//	(pg_advisory_unlock) or when that session closes. TryAcquire pins the connection and
//	tries the lock; Release unlocks and returns the connection.
//
//	Crash-safety / failover: if the leader process dies, its connection drops and Postgres
//	releases the session advisory lock automatically — so another replica's TryAcquire
//	then wins. No lease renewal or TTL is needed; the lock IS the lease, fenced by the TCP
//	session. A brief two-leader overlap during a failover is tolerated downstream (the
//	consumer's idempotency marker dedups), exactly as the seam documents.
//
// CLEAN CORE: this lives in the gormtx ADAPTER (it needs a *sql.DB / driver, which the
// clean core forbids), implementing the backend-neutral events.Leader interface. The
// relay only ever sees events.Leader.
package gormtx

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"sync"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/events"
)

// PGAdvisoryLeader is the PostgreSQL advisory-lock events.Leader. One PGAdvisoryLeader
// instance corresponds to one relay; construct it with a lock NAME shared by every
// replica of the same service (so they contend for the SAME lock) and the service's
// *sql.DB. The relay calls TryAcquire before pumping and Release on stop.
type PGAdvisoryLeader struct {
	db  *sql.DB
	key int64 // the advisory-lock key derived from the lock name

	mu   sync.Mutex
	conn *sql.Conn // the pinned session holding the lock while leader; nil when not held
}

// DefaultRelayLockName is the advisory-lock name a service's relays share by default
// (override with NewPGAdvisoryLeader's name when a process runs more than one logical
// relay). All replicas of one service must pass the SAME name to contend for one lock.
const DefaultRelayLockName = "devedge-sdk/relay/outbox-pump"

// NewPGAdvisoryLeaderFromGorm derives the underlying *sql.DB from a *gorm.DB and returns
// a PGAdvisoryLeader keyed by name — the convenient constructor for a service that
// already holds the GORM handle its repositories use.
func NewPGAdvisoryLeaderFromGorm(db *gorm.DB, name string) (*PGAdvisoryLeader, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("gormtx: resolve *sql.DB for advisory leader: %w", err)
	}
	return NewPGAdvisoryLeader(sqlDB, name), nil
}

// NewPGAdvisoryLeader returns a PostgreSQL advisory-lock leader over db, contending on
// the lock derived from name. An empty name uses [DefaultRelayLockName].
func NewPGAdvisoryLeader(db *sql.DB, name string) *PGAdvisoryLeader {
	if name == "" {
		name = DefaultRelayLockName
	}
	return &PGAdvisoryLeader{db: db, key: advisoryKey(name)}
}

// advisoryKey hashes a lock name to the int64 key pg_advisory_lock takes. FNV-1a over the
// name gives a stable, well-spread key; collisions only mean two unrelated names would
// share a lock, which a distinct name avoids.
func advisoryKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64()) // reinterpret as signed; pg_advisory_lock takes a bigint
}

// TryAcquire implements events.Leader: pin a dedicated session connection and try the
// session advisory lock on it. A true return means this caller now holds leadership
// cluster-wide; false means another replica's session holds the lock. A holder that calls
// it again returns true (it still holds its pinned session lock) without re-locking.
//
// We use pg_try_advisory_lock (non-blocking): it returns immediately with true/false
// rather than waiting, so a non-leader replica idles its tick instead of blocking a
// connection waiting for the lock.
func (l *PGAdvisoryLeader) TryAcquire(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		// Already the leader on our pinned session — a re-acquire by the holder is true.
		return true, nil
	}
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("gormtx: pin advisory-lock session: %w", err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", l.key).Scan(&got); err != nil {
		_ = conn.Close()
		return false, fmt.Errorf("gormtx: pg_try_advisory_lock: %w", err)
	}
	if !got {
		// Another session holds the lock. Return our connection to the pool and idle.
		_ = conn.Close()
		return false, nil
	}
	// We hold the lock; keep the session pinned for the lock's lifetime.
	l.conn = conn
	return true, nil
}

// Release implements events.Leader: unlock the session advisory lock on our pinned
// session and return the connection to the pool, so another replica can acquire it. It is
// a no-op if not currently held. Even if pg_advisory_unlock errors, closing the pinned
// connection releases the session lock (Postgres releases session advisory locks on
// session end), so leadership is always relinquished.
func (l *PGAdvisoryLeader) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil
	}
	conn := l.conn
	l.conn = nil
	var unlockErr error
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", l.key); err != nil {
		unlockErr = fmt.Errorf("gormtx: pg_advisory_unlock: %w", err)
	}
	// Closing the pinned connection releases the session advisory lock unconditionally
	// (the session ends), which is the real fence — so a failed unlock query still frees it.
	if cerr := conn.Close(); cerr != nil && unlockErr == nil {
		unlockErr = fmt.Errorf("gormtx: close advisory-lock session: %w", cerr)
	}
	return unlockErr
}

// compile-time check.
var _ events.Leader = (*PGAdvisoryLeader)(nil)
