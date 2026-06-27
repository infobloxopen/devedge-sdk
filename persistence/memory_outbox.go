package persistence

import (
	"context"
	"sort"
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
// aggregate change: the runner holds the store's lock and snapshots its rows for the
// duration of fn, so a committed Publish keeps its row and a rolled-back one discards
// it (F032 AC-1) — exactly as the ent store's Append-through-*ent.Tx does for the SQL
// backend.
//
// F033 WRITE-ONLY: the store is write-only. The ONLY writes are Append (the
// producer's transactional insert) and [MemoryOutboxStore.DropPartitionsBefore] (the
// dev-backend model of partition-drop retention: forget rows older than t). There is
// no claim, lease, delivered-mark, or per-row delete — the in-process dispatcher reads
// the rows forward via [MemoryOutboxStore.ReadAfter] and keeps its position in a
// separate [MemoryOutboxCursorStore]; an outbox row is never mutated after it is
// appended.
type MemoryOutboxStore struct {
	mu    sync.Mutex
	rows  []*OutboxRecord // append order; ReadAfter scans this in (created_time, id) order
	order uint64
}

// NewMemoryOutboxStore returns an in-memory write-only OutboxStore.
func NewMemoryOutboxStore() *MemoryOutboxStore {
	return &MemoryOutboxStore{
		order: memOutboxOrderBase + memOutboxOrderSeq.Add(1),
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
	return rows
}

func (s *MemoryOutboxStore) restoreForTx(snap any) {
	s.rows = snap.([]*OutboxRecord)
}

// inThisTx reports whether ctx carries a MemoryTxRunner transaction this store is
// enrolled in. When true the runner already holds s.mu for the whole transaction, so
// Append must NOT re-lock (s.mu is not reentrant) — mirroring MemoryRepository.
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
// dual-write guard (F032 D-1); package events.Publish enforces the same via RequireTx,
// this is the store-level backstop. This is the ONLY write the producer makes.
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

// ReadAfter implements [OutboxStore]: return up to limit rows strictly after cursor in
// (created_time, id) order, WITHOUT mutating any row. It is the non-destructive
// forward scan the in-process dispatcher consumes; the dispatcher advances its own
// cursor in a sidecar and never writes back here. A read is its own short critical
// section (not part of an aggregate tx), so it locks directly.
func (s *MemoryOutboxStore) ReadAfter(ctx context.Context, cursor OutboxCursor, limit int) ([]*OutboxRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Sort a copy by (created_time, id) so the scan is total and deterministic even if
	// rows were appended out of created_time order (e.g. a backdated CreatedTime).
	sorted := make([]*OutboxRecord, len(s.rows))
	copy(sorted, s.rows)
	sort.Slice(sorted, func(i, j int) bool { return outboxLess(sorted[i], sorted[j]) })

	out := make([]*OutboxRecord, 0, limit)
	for _, r := range sorted {
		if len(out) >= limit {
			break
		}
		if !cursorBefore(cursor, r) {
			continue // at or before the cursor: already consumed
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

// outboxLess orders two records by (created_time, id) — the forward-cursor order.
func outboxLess(a, b *OutboxRecord) bool {
	if a.CreatedTime.Equal(b.CreatedTime) {
		return a.ID < b.ID
	}
	return a.CreatedTime.Before(b.CreatedTime)
}

// cursorBefore reports whether the cursor strictly precedes record r — i.e. r is
// "after the cursor" and so should be returned by ReadAfter. A zero cursor precedes
// every row (read from the beginning).
func cursorBefore(c OutboxCursor, r *OutboxRecord) bool {
	if c.IsZero() {
		return true
	}
	if c.CreatedTime.Equal(r.CreatedTime) {
		return c.ID < r.ID
	}
	return c.CreatedTime.Before(r.CreatedTime)
}

// DropPartitionsBefore implements [OutboxRetention] for the dev backend: it forgets
// every row whose CreatedTime is strictly older than t (the in-memory model of an SQL
// partition drop) and returns how many rows it removed. This is the ONLY path that
// removes data from the write-only store; nothing else ever deletes.
func (s *MemoryOutboxStore) DropPartitionsBefore(ctx context.Context, t time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.rows[:0:0]
	dropped := 0
	for _, r := range s.rows {
		if r.CreatedTime.Before(t) {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	s.rows = kept
	return dropped, nil
}

// Pending returns the ids of every stored (not-yet-dropped) row in (created_time, id)
// order. Under the write-only model there is no per-row "delivered" state — the
// dispatcher tracks its position in a sidecar — so every appended row that has not been
// dropped by retention is "pending" in the outbox. A test/introspection helper.
func (s *MemoryOutboxStore) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]*OutboxRecord, len(s.rows))
	copy(rows, s.rows)
	sort.Slice(rows, func(i, j int) bool { return outboxLess(rows[i], rows[j]) })
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// All returns a copy of every stored row in (created_time, id) order, for
// tests/introspection. Under the write-only model the row count only grows until a
// DropPartitionsBefore.
func (s *MemoryOutboxStore) All() []*OutboxRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*OutboxRecord, len(s.rows))
	for i, r := range s.rows {
		cp := *r
		out[i] = &cp
	}
	sort.Slice(out, func(i, j int) bool { return outboxLess(out[i], out[j]) })
	return out
}

// MemoryOutboxCursorStore is the in-memory [OutboxCursorStore] sidecar for the
// in-process dispatcher: it holds, per named cursor, the forward position, the
// head-of-line failure count, and a dead-letter list. It is the dev-backend twin of a
// SQL sidecar table. It is deliberately independent of the write-only
// [MemoryOutboxStore] — the dispatcher records ALL its progress here, never in the
// outbox.
type MemoryOutboxCursorStore struct {
	mu      sync.Mutex
	cursors map[string]cursorState
	dead    []DeadLetterRecord
}

type cursorState struct {
	cursor       OutboxCursor
	headFailures int
}

// DeadLetterRecord is one parked poison event in the in-memory sidecar (for
// tests/introspection): the event that failed maxAttempts at the cursor head before
// the dispatcher advanced past it.
type DeadLetterRecord struct {
	CursorName string
	EventID    string
	EventType  string
	Reason     string
	Position   OutboxCursor
}

// NewMemoryOutboxCursorStore returns an empty in-memory cursor sidecar.
func NewMemoryOutboxCursorStore() *MemoryOutboxCursorStore {
	return &MemoryOutboxCursorStore{cursors: make(map[string]cursorState)}
}

// LoadCursor implements [OutboxCursorStore]: return the saved position + head-failure
// count for name (zero/0 if never saved).
func (s *MemoryOutboxCursorStore) LoadCursor(ctx context.Context, name string) (OutboxCursor, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.cursors[name]
	return st.cursor, st.headFailures, nil
}

// SaveCursor implements [OutboxCursorStore]: durably record the position + head-failure
// count for name.
func (s *MemoryOutboxCursorStore) SaveCursor(ctx context.Context, name string, cursor OutboxCursor, headFailures int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors[name] = cursorState{cursor: cursor, headFailures: headFailures}
	return nil
}

// DeadLetter implements [OutboxCursorStore]: park a poison event in the sidecar.
func (s *MemoryOutboxCursorStore) DeadLetter(ctx context.Context, name string, rec *OutboxRecord, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = append(s.dead, DeadLetterRecord{
		CursorName: name,
		EventID:    rec.ID,
		EventType:  rec.EventType,
		Reason:     reason,
		Position:   OutboxCursor{CreatedTime: rec.CreatedTime, ID: rec.ID},
	})
	return nil
}

// DeadLettered returns a copy of the parked poison events, for tests/introspection.
func (s *MemoryOutboxCursorStore) DeadLettered() []DeadLetterRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DeadLetterRecord, len(s.dead))
	copy(out, s.dead)
	return out
}

// compile-time checks.
var (
	_ OutboxStore         = (*MemoryOutboxStore)(nil)
	_ OutboxRetention     = (*MemoryOutboxStore)(nil)
	_ MemoryRepositoryFor = (*MemoryOutboxStore)(nil)
	_ OutboxCursorStore   = (*MemoryOutboxCursorStore)(nil)
)
