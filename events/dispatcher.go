package events

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// Handler reacts to a delivered domain event by changing ANOTHER aggregate. The
// dispatcher runs each handler in its own [persistence.TxRunner.Atomically] (see
// [Dispatcher.Run]), so a handler should treat ctx as already transactional and do
// a single-aggregate write — the reaction is itself a safe aggregate write (F032
// G-4). Returning an error leaves the event undelivered so it is re-tried
// (at-least-once); handlers MUST therefore be idempotent (F032 D-4).
type Handler func(ctx context.Context, evt Event) error

// ErrAlreadyApplied is returned by [IdempotencyStore.Record] when key has already
// been recorded. It is the exactly-once guard: Record is called INSIDE the
// handler's transaction, so a concurrent (or lapsed-lease) double-delivery races to
// record the SAME (event, handler) marker; the loser gets ErrAlreadyApplied and its
// whole handler transaction — effect AND marker — rolls back, leaving the side
// effect applied exactly once even though the event was delivered more than once
// (F032 AC-2). Without an in-tx unique marker, the bare Seen→run→Record check is a
// check-then-act race that double-fires the effect on a double-claim.
var ErrAlreadyApplied = errors.New("events: idempotency key already applied")

// IdempotencyStore records which (event, handler) pairs have already been applied,
// so an at-least-once redelivery is a no-op (F032 D-4 / AC-2). It is the dedup
// precedent (AIP-155, middleware.DeduplicationStore) reused for events.
//
// Correctness contract (the part that makes the effect exactly-once, not just
// usually-once): [Record] is invoked INSIDE the handler's [persistence.TxRunner]
// transaction, on the tx-bound ctx, so the marker commits ATOMICALLY with the
// handler's aggregate write. Record MUST be unique: a second Record of an
// already-recorded key returns [ErrAlreadyApplied] rather than silently succeeding.
// That uniqueness is what serializes a concurrent double-apply — exactly one
// transaction commits (effect + marker), the other rolls back on ErrAlreadyApplied.
// [Seen] is only a fast-path pre-check to skip re-running a long-committed handler;
// correctness does NOT depend on it.
//
// The dev default is in-memory ([MemoryIdempotencyStore]), backed by a
// [persistence.MemoryRepository] so it enlists in a [persistence.MemoryTxRunner]
// transaction (pass it to [persistence.NewMemoryTxRunner] alongside the handler's
// repositories). A production store persists the marker in a unique row written in
// the handler's own backend tx.
type IdempotencyStore interface {
	// Seen reports whether key has already been recorded (fast-path pre-check).
	Seen(ctx context.Context, key string) (bool, error)
	// Record marks key as applied, inside the handler's transaction. It returns
	// [ErrAlreadyApplied] if key is already recorded so the caller's transaction
	// rolls back the duplicate effect.
	Record(ctx context.Context, key string) error
}

// idemMarker is the row stored per applied (event, handler) key. Keyed by the
// idempotency key so a duplicate Record is a unique-key conflict.
type idemMarker struct{ Key string }

// MemoryIdempotencyStore is the in-memory [IdempotencyStore] dev default. It is
// backed by a [persistence.MemoryRepository] so its marker write enlists in the
// same [persistence.MemoryTxRunner] transaction as the handler's aggregate write —
// pass it to [persistence.NewMemoryTxRunner] (via [MemoryIdempotencyStore.TxParticipant])
// so a concurrent double-apply is serialized by the unique marker and the losing
// transaction (effect + marker) rolls back together (F032 AC-2).
type MemoryIdempotencyStore struct {
	repo *persistence.MemoryRepository[idemMarker, string]
}

// NewMemoryIdempotencyStore returns an empty in-memory IdempotencyStore.
func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{
		repo: persistence.NewMemoryRepository(func(m idemMarker) string { return m.Key }),
	}
}

// TxParticipant exposes the backing repository so a caller can enroll the marker
// store in the handler's [persistence.MemoryTxRunner] — required for the marker to
// commit/roll back atomically with the handler's aggregate write (the exactly-once
// guarantee). The [Dispatcher] enrolls it automatically for a MemoryTxRunner.
func (s *MemoryIdempotencyStore) TxParticipant() persistence.MemoryRepositoryFor {
	return s.repo
}

// Seen implements [IdempotencyStore] (fast-path pre-check).
func (s *MemoryIdempotencyStore) Seen(ctx context.Context, key string) (bool, error) {
	if _, err := s.repo.Get(ctx, key); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Record implements [IdempotencyStore]: insert the marker inside the handler's tx.
// A duplicate key is a [persistence.ErrConflict] from the repository, surfaced as
// [ErrAlreadyApplied] so the caller rolls the duplicate effect back.
func (s *MemoryIdempotencyStore) Record(ctx context.Context, key string) error {
	if _, err := s.repo.Create(ctx, idemMarker{Key: key}); err != nil {
		if errors.Is(err, persistence.ErrConflict) {
			return ErrAlreadyApplied
		}
		return err
	}
	return nil
}

// compile-time check.
var _ IdempotencyStore = (*MemoryIdempotencyStore)(nil)

// Dispatcher delivers committed outbox events to registered handlers at-least-once
// (F032 G-3). The dev default is an in-process poller (F032 D-3): it claims
// undelivered rows from the [persistence.OutboxStore] (a claimed-flag/lease claim,
// NOT SELECT ... FOR UPDATE SKIP LOCKED — ent sql/lock is off in this repo), runs
// each registered handler for the event's type in its own [persistence.TxRunner]
// transaction, and marks the event delivered only once EVERY handler succeeded.
//
// Delivery semantics:
//   - At-least-once: a handler error (or crash) leaves the event undelivered, so a
//     later claim re-delivers it. Handlers MUST be idempotent.
//   - Idempotency: each (event id, handler) pair is recorded in the
//     [IdempotencyStore] once its tx commits; a redelivery skips an already-applied
//     pair, so a duplicate delivery is a no-op (AC-2).
//   - Per-event ordering only: events for one aggregate claim in append order, but
//     no global order is promised (F032 non-goal).
type Dispatcher struct {
	store    persistence.OutboxStore
	tx       persistence.TxRunner
	idem     IdempotencyStore
	mu       sync.RWMutex
	handlers map[string][]registeredHandler
}

type registeredHandler struct {
	name string
	fn   Handler
}

// NewDispatcher returns a Dispatcher that claims from store, runs handlers in tx,
// and dedups via idem. tx is the [persistence.TxRunner] for the aggregates the
// handlers write (each handler runs inside it). idem may be nil to use a fresh
// in-memory [MemoryIdempotencyStore].
func NewDispatcher(store persistence.OutboxStore, tx persistence.TxRunner, idem IdempotencyStore) *Dispatcher {
	if idem == nil {
		idem = NewMemoryIdempotencyStore()
	}
	// Exactly-once requires the idempotency marker to commit in the SAME tx as the
	// handler's aggregate write. For the in-memory dev default that means the marker
	// store must be a participant of the handler's MemoryTxRunner; enroll it
	// automatically so a caller need not remember to (AC-2). For other backends the
	// caller wires an idem store that records in the handler's own backend tx.
	if mem, ok := idem.(*MemoryIdempotencyStore); ok {
		if mtx, ok := tx.(*persistence.MemoryTxRunner); ok {
			tx = mtx.WithParticipants(mem.TxParticipant())
		}
	}
	return &Dispatcher{
		store:    store,
		tx:       tx,
		idem:     idem,
		handlers: make(map[string][]registeredHandler),
	}
}

// Subscribe registers fn as a handler for events of eventType (F032 G-4). name
// identifies the handler in the idempotency key so two handlers of the same event
// dedup independently. Multiple handlers may subscribe to one type; all must
// succeed for the event to be marked delivered.
func (d *Dispatcher) Subscribe(eventType, name string, fn Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[eventType] = append(d.handlers[eventType], registeredHandler{name: name, fn: fn})
}

func (d *Dispatcher) handlersFor(eventType string) []registeredHandler {
	d.mu.RLock()
	defer d.mu.RUnlock()
	hs := d.handlers[eventType]
	out := make([]registeredHandler, len(hs))
	copy(out, hs)
	return out
}

// idempotencyKey is the per-(event, handler) dedup key. Keying on the event id
// (D-4) plus the handler name lets a multi-handler event redeliver only the
// handlers that have not yet committed.
func idempotencyKey(eventID, handlerName string) string {
	return eventID + "\x00" + handlerName
}

// deliver runs every handler subscribed to evt.Type. For each handler it runs the
// reaction AND records the idempotency marker in ONE transaction (G-4), so the
// effect and the marker commit or roll back together. The exactly-once guard is the
// in-tx unique marker: on a concurrent or lapsed-lease double-delivery, both copies
// race to record the same (event, handler) marker; exactly one commits (effect +
// marker) and the other gets [ErrAlreadyApplied] and rolls its whole transaction
// back, so the side effect runs once even though the event was delivered twice
// (AC-2). [IdempotencyStore.Seen] is only a fast-path pre-check that skips re-running
// a long-committed handler; it is NOT relied on for correctness.
//
// deliver returns nil only when every handler is applied (committed now or
// previously), so the caller marks the event delivered exactly once all reactions
// are durable.
func (d *Dispatcher) deliver(ctx context.Context, evt Event) error {
	// Scope the handler to the EVENT's tenant. The dispatcher claims across all
	// tenants (the outbox deliberately has no TenantMixin), so the caller's ctx
	// tenant is unrelated to the event's. A tenant-scoped repository reads
	// middleware.TenantIDFromContext to filter its writes; running the handler on
	// the dispatcher's tenant (or none) would let it read/revoke ANOTHER tenant's
	// aggregates — a cross-tenant write. Inject the event's account so the reaction
	// targets exactly the tenant that emitted it.
	if evt.AccountID != "" {
		ctx = middleware.WithTenantID(ctx, evt.AccountID)
	}
	for _, h := range d.handlersFor(evt.Type) {
		key := idempotencyKey(evt.ID, h.name)
		// Fast path: skip a handler whose marker is already committed, so a redelivery
		// of a long-applied event does not re-open a transaction. Correctness does not
		// depend on this — the in-tx Record below is the real guard against a racy
		// double-apply (two deliveries can both pass Seen before either records).
		seen, err := d.idem.Seen(ctx, key)
		if err != nil {
			return fmt.Errorf("idempotency check: %w", err)
		}
		if seen {
			continue // already applied — redelivery no-op (AC-2)
		}
		// Run the reaction AND record the marker in ONE aggregate transaction (G-4):
		// the marker is unique, so a concurrent double-apply collides and the loser's
		// whole tx (effect + marker) rolls back — the side effect commits exactly once.
		// A handler error (or a lost race) leaves the event undelivered for a later
		// retry (at-least-once).
		txErr := d.tx.Atomically(ctx, func(ctx context.Context) error {
			if herr := h.fn(ctx, evt); herr != nil {
				return herr
			}
			// Record on the tx-bound ctx so the marker commits with the effect.
			return d.idem.Record(ctx, key)
		})
		if txErr != nil {
			if errors.Is(txErr, ErrAlreadyApplied) {
				// A concurrent (or earlier, lease-lapsed) delivery already committed this
				// (event, handler); our transaction rolled the duplicate effect back. The
				// handler IS applied — treat as a no-op success and move on (AC-2).
				continue
			}
			return fmt.Errorf("handler %q for %q: %w", h.name, evt.Type, txErr)
		}
	}
	return nil
}

// RunOnce claims up to limit undelivered events and delivers each. It returns the
// number of events successfully delivered (all their handlers applied). An event
// whose handler errors is left undelivered for a later RunOnce (at-least-once
// retry); RunOnce does not fail the whole batch for one bad event. A poller calls
// RunOnce on a tick; tests call it directly to drive delivery deterministically.
func (d *Dispatcher) RunOnce(ctx context.Context, limit int) (delivered int, err error) {
	claimed, err := d.store.ClaimUndelivered(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("claim undelivered: %w", err)
	}
	for _, rec := range claimed {
		evt := eventFromRecord(rec)
		if derr := d.deliver(ctx, evt); derr != nil {
			// Release the lease so the retry is prompt rather than lease-delayed; the
			// row stays undelivered and will be re-claimed. Surface the first error so a
			// caller can log it, but keep processing the batch (one bad event must not
			// stall the others — at-least-once).
			_ = d.store.Release(ctx, rec.ID)
			if err == nil {
				err = derr
			}
			continue
		}
		if merr := d.store.MarkDelivered(ctx, rec.ID); merr != nil {
			if err == nil {
				err = fmt.Errorf("mark delivered %s: %w", rec.ID, merr)
			}
			continue
		}
		delivered++
	}
	return delivered, err
}

// Poll runs RunOnce on every tick of interval until ctx is cancelled. It is the
// in-process poller dev default; a caller wires it as a goroutine. Errors from a
// tick are reported through onErr (nil to ignore) so one bad event does not stop
// the loop.
func (d *Dispatcher) Poll(ctx context.Context, interval time.Duration, batch int, onErr func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	if batch <= 0 {
		batch = 100
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := d.RunOnce(ctx, batch); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// eventFromRecord rebuilds the Event a handler sees from a claimed outbox row.
func eventFromRecord(rec *persistence.OutboxRecord) Event {
	return Event{
		ID:            rec.ID,
		Type:          rec.EventType,
		AggregateType: rec.AggregateType,
		AggregateID:   rec.AggregateID,
		AccountID:     rec.AccountID,
		Payload:       rec.Payload,
	}
}
