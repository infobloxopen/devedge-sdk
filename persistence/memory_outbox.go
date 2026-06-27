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
// F033: the store is APPEND-ONLY. Rows are never deleted or delivered-marked on the
// dispatch path; claim eligibility is attempts-based (a row drops out of claims once
// its Attempts reaches the dispatcher's maxAttempts, the poison cutoff), and the
// only path that removes data is [MemoryOutboxStore.DropPartitionsBefore] (the
// dev-backend model of partition-drop retention: forget rows older than t).
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
// F033: eligibility is attempts-based, not delivered-time-based. A row that has been
// attempted maxAttempts times is poison and is no longer returned (the poison
// cutoff); a successfully delivered row keeps being eligible until the cutoff but its
// re-delivery is a harmless no-op (the handler markers dedup it), and it is
// eventually aged out by DropPartitionsBefore.
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

// MarkDelivered implements [OutboxStore]: a NO-OP under the F033 append-only model.
// Delivery truth is the idempotency marker recorded in the handler's transaction,
// not a row write; the store never mutates delivery state and never deletes a row.
func (s *MemoryOutboxStore) MarkDelivered(ctx context.Context, id string) error {
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

// Pending returns the ids of rows still eligible for claim (Attempts below the
// default cutoff), in append order. Under the F033 append-only model the store does
// not track delivery (the idempotency marker is the delivery truth), so Pending
// reflects claim-eligibility, not delivered-state; it is a test/introspection helper.
func (s *MemoryOutboxStore) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.rows))
	for _, r := range s.rows {
		if r.Attempts < DefaultMaxOutboxAttempts {
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
