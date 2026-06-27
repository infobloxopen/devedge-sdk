package persistence

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// memOutboxOrderBase keeps the in-memory outbox store's lock-order key from
// colliding with a MemoryRepository's (which counts up from a small base). The
// store and the repositories it commits alongside must have a stable, distinct
// ordering so a single Atomically that touches both acquires locks deterministically.
var memOutboxOrderSeq atomic.Uint64

const memOutboxOrderBase = 1 << 32

// MemoryOutboxStore is the in-memory [OutboxStore] dev default. It is also a
// [MemoryRepositoryFor] participant, so passing it to [NewMemoryTxRunner] alongside
// the aggregate's repositories makes Append enlist in the SAME transaction as the
// aggregate change: the runner holds the store's lock and snapshots its rows for
// the duration of fn, so a committed Publish keeps its row and a rolled-back one
// discards it (F032 AC-1) — exactly as the ent store's Append-through-*ent.Tx does
// for the SQL backend.
//
// F033: the store is APPEND-ONLY — rows are never deleted on the dispatch path. The
// only mutation the dispatch path makes is a single terminal delivered-mark
// ([MemoryOutboxStore.MarkDelivered] sets the row's DeliveredTime once every handler
// applied); a delivered row is then excluded from claims, so it is never re-leased or
// re-attempted (no per-poll churn) and never drifts into the poison cutoff. A row
// drops out of claims once its Attempts reaches maxAttempts (the poison cutoff) even
// if never delivered. The only path that removes data is
// [MemoryOutboxStore.DropPartitionsBefore] (the dev-backend model of partition-drop
// retention: forget rows older than t).
type MemoryOutboxStore struct {
	mu       sync.Mutex
	rows     []*OutboxRecord      // append order; the dispatcher claims from the head
	leased   map[string]time.Time // id -> lease expiry; a leased row is hidden from claims
	leaseTTL time.Duration
	order    uint64
}

// NewMemoryOutboxStore returns an in-memory OutboxStore. leaseTTL is how long a
// claimed row stays hidden from a competing claim before it may be re-leased (the
// claimed-flag/lease strategy of F032 D-3); a non-positive value uses a sane
// default.
func NewMemoryOutboxStore(leaseTTL time.Duration) *MemoryOutboxStore {
	if leaseTTL <= 0 {
		leaseTTL = 30 * time.Second
	}
	return &MemoryOutboxStore{
		leased:   make(map[string]time.Time),
		leaseTTL: leaseTTL,
		order:    memOutboxOrderBase + memOutboxOrderSeq.Add(1),
	}
}

// --- memParticipant: lets the store enlist in a MemoryTxRunner transaction ---

func (s *MemoryOutboxStore) lockForTx()        { s.mu.Lock() }
func (s *MemoryOutboxStore) unlockForTx()      { s.mu.Unlock() }
func (s *MemoryOutboxStore) lockOrder() uint64 { return s.order }

func (s *MemoryOutboxStore) snapshotForTx() any {
	rows := make([]*OutboxRecord, len(s.rows))
	for i, r := range s.rows {
		cp := *r
		rows[i] = &cp
	}
	leased := make(map[string]time.Time, len(s.leased))
	maps.Copy(leased, s.leased)
	return memOutboxSnapshot{rows: rows, leased: leased}
}

func (s *MemoryOutboxStore) restoreForTx(snap any) {
	ss := snap.(memOutboxSnapshot)
	s.rows = ss.rows
	s.leased = ss.leased
}

type memOutboxSnapshot struct {
	rows   []*OutboxRecord
	leased map[string]time.Time
}

// inThisTx reports whether ctx carries a MemoryTxRunner transaction this store is
// enrolled in. When true the runner already holds s.mu for the whole transaction,
// so Append must NOT re-lock (s.mu is not reentrant) — mirroring MemoryRepository.
func (s *MemoryOutboxStore) inThisTx(ctx context.Context) bool {
	if set, ok := memTxSetFromContext(ctx); ok {
		return set.has(s)
	}
	return false
}

// Append implements [OutboxStore]: record rec inside the ctx transaction. When ctx
// carries a MemoryTxRunner transaction this store is enrolled in, the row is added
// under the runner-held lock so it is kept on commit and dropped on rollback (the
// snapshot above is restored). Append outside any transaction is rejected — the
// dual-write guard (F032 D-1); package events.Publish enforces the same via
// RequireTx, this is the store-level backstop.
func (s *MemoryOutboxStore) Append(ctx context.Context, rec *OutboxRecord) error {
	if rec.CreatedTime.IsZero() {
		rec.CreatedTime = time.Now()
	}
	cp := *rec
	if s.inThisTx(ctx) {
		s.rows = append(s.rows, &cp) // runner holds the lock
		return nil
	}
	// Not enrolled in this store's transaction. Fail closed rather than write on a
	// separate "connection" and reintroduce the dual-write the outbox prevents.
	if err := RequireTx(ctx); err != nil {
		return err
	}
	// A tx is on ctx but it is not THIS store's (the store was not passed to the
	// runner). Treat that as a misconfiguration the same way: the write would not be
	// atomic with the aggregate change.
	return ErrNoTransaction
}

// ClaimUndelivered implements [OutboxStore]: lease up to limit rows still eligible
// for dispatch — Attempts < maxAttempts and lease lapsed — to the caller, stamping a
// fresh lease and bumping Attempts. A claim is its own short critical section (not
// part of an aggregate tx), so it locks directly.
//
// F033: a delivered row (DeliveredTime set) is EXCLUDED — a successfully delivered
// event is never re-claimed or re-attempted, so the happy path does zero per-poll
// writes and a delivered event never drifts into the poison cutoff. A row attempted
// maxAttempts times without ever delivering is poison and is no longer returned (the
// poison cutoff). Both are eventually aged out by DropPartitionsBefore.
func (s *MemoryOutboxStore) ClaimUndelivered(ctx context.Context, maxAttempts, limit int) ([]*OutboxRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxOutboxAttempts
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]*OutboxRecord, 0, limit)
	for _, r := range s.rows {
		if len(out) >= limit {
			break
		}
		if r.DeliveredTime != nil {
			continue // already delivered: never re-claimed (no churn)
		}
		if r.Attempts >= maxAttempts {
			continue // poison: past the cutoff, no longer claimed
		}
		if exp, leased := s.leased[r.ID]; leased && now.Before(exp) {
			continue // still leased to another claim
		}
		s.leased[r.ID] = now.Add(s.leaseTTL)
		r.Attempts++
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

// MarkDelivered implements [OutboxStore]: stamp the row's DeliveredTime ONCE so it is
// excluded from every future claim (F033 churn-avoidance — no per-poll re-lease of a
// delivered event). It never deletes the row; a second mark of an already-delivered
// row is a no-op, and an unknown id is ignored.
func (s *MemoryOutboxStore) MarkDelivered(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.ID == id {
			if r.DeliveredTime == nil {
				t := time.Now()
				r.DeliveredTime = &t
			}
			delete(s.leased, id) // delivered rows need no lease
			return nil
		}
	}
	return nil
}

// Release implements [OutboxStore]: drop the lease on id so a re-claim is immediate.
// A no-op for an unknown id. It does NOT delete the row (append-only).
func (s *MemoryOutboxStore) Release(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leased, id)
	return nil
}

// DropPartitionsBefore implements [OutboxRetention] for the dev backend: it forgets
// every row whose CreatedTime is strictly older than t (the in-memory model of an
// SQL partition drop) and returns how many rows it removed. This is the ONLY path
// that removes data from the append-only store; the dispatch loop never deletes.
func (s *MemoryOutboxStore) DropPartitionsBefore(ctx context.Context, t time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.rows[:0:0]
	dropped := 0
	for _, r := range s.rows {
		if r.CreatedTime.Before(t) {
			delete(s.leased, r.ID)
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	s.rows = kept
	return dropped, nil
}

// Pending returns the ids of rows still eligible for claim (not yet delivered and
// Attempts below the default cutoff), in append order. It is a test/introspection
// helper that mirrors ClaimUndelivered's eligibility filter.
func (s *MemoryOutboxStore) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.rows))
	for _, r := range s.rows {
		if r.DeliveredTime == nil && r.Attempts < DefaultMaxOutboxAttempts {
			out = append(out, r.ID)
		}
	}
	return out
}

// All returns a copy of every stored row in append order, for tests/introspection.
// Under the append-only model the row count only grows until a DropPartitionsBefore.
func (s *MemoryOutboxStore) All() []*OutboxRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*OutboxRecord, len(s.rows))
	for i, r := range s.rows {
		cp := *r
		out[i] = &cp
	}
	return out
}

// compile-time checks.
var (
	_ OutboxStore         = (*MemoryOutboxStore)(nil)
	_ OutboxRetention     = (*MemoryOutboxStore)(nil)
	_ MemoryRepositoryFor = (*MemoryOutboxStore)(nil)
)
