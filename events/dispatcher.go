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

// DefaultCursorName is the sidecar cursor name a Dispatcher uses when the caller does
// not set one with [WithCursorName]. A service runs a single in-process dispatcher
// instance (the SDK assumption — the external WAL/queue consumer is the scale-out
// path), so one named cursor per dispatcher is sufficient.
const DefaultCursorName = "default"

// Dispatcher delivers committed outbox events to registered handlers at-least-once
// (F032 G-3) for SAME-DB cross-aggregate reactions (e.g. iam UserSuspended → revoke
// the user's API keys). The dev default is an in-process FORWARD-CURSOR consumer
// (F033): the outbox table is WRITE-ONLY, so the dispatcher never claims, leases,
// marks, or deletes an outbox row. Instead it keeps its OWN position in a sidecar
// ([persistence.OutboxCursorStore]), reads the outbox forward
// ([persistence.OutboxStore.ReadAfter] — `(created_time, id) > cursor ORDER BY
// created_time, id LIMIT n`), delivers each event to its handlers in commit/created
// order, and ADVANCES the sidecar cursor past it. Cross-server delivery (publishing to
// queues) is OUT OF SCOPE — that is an external project that tails the outbox via the
// [persistence.OutboxCDCConsumer] seam.
//
// Exactly-once effect: delivery is AT-LEAST-ONCE (a crash between deliver and
// cursor-advance re-delivers the same event), guarded into an exactly-once EFFECT by
// the per-(event, handler) [IdempotencyStore] marker, which commits INSIDE the
// handler's transaction. A re-delivery whose marker is already committed is a no-op.
// This is robust even if cursor concurrency is imperfect; the SDK assumes a single
// dispatcher instance per service for the cursor regardless.
//
// Delivery semantics:
//   - Forward cursor: events are read and delivered in (created_time, id) order
//     (commit/created order). Per-aggregate ordering is therefore preserved.
//   - At-least-once + idempotency = exactly-once effect: a handler error (or crash)
//     leaves the cursor un-advanced, so a later RunOnce re-delivers; the idempotency
//     marker makes a re-delivery a no-op. Handlers MUST be idempotent.
//   - Write-only outbox: the dispatcher NEVER mutates an outbox row — all progress is
//     the sidecar cursor. A delivered event is never re-touched or re-written.
//   - Poison / head-of-line: the head event is the oldest un-consumed event. If it
//     fails delivery, the cursor does not advance (it would skip the gap), so the
//     batch stops at the head — bounded head-of-line blocking. After maxAttempts
//     consecutive failures on the SAME head event, the dispatcher records it to the
//     sidecar dead-letter and advances PAST it, so one permanently-failing event does
//     not wedge the stream forever (F033 AC-3). The head-failure count lives in the
//     sidecar, not in an outbox attempts column.
type Dispatcher struct {
	store       persistence.OutboxStore
	cursors     persistence.OutboxCursorStore
	cursorName  string
	tx          persistence.TxRunner
	idem        IdempotencyStore
	maxAttempts int
	mu          sync.RWMutex
	handlers    map[string][]registeredHandler
}

type registeredHandler struct {
	name string
	fn   Handler
}

// DispatcherOption configures a Dispatcher.
type DispatcherOption func(*Dispatcher)

// WithMaxAttempts sets the poison cutoff: after this many consecutive failed
// deliveries of the SAME head event, the dispatcher dead-letters it (in the sidecar)
// and advances the cursor past it (F033). A non-positive value keeps the default
// ([persistence.DefaultMaxOutboxAttempts]).
func WithMaxAttempts(n int) DispatcherOption {
	return func(d *Dispatcher) {
		if n > 0 {
			d.maxAttempts = n
		}
	}
}

// WithCursorName sets the sidecar cursor name this dispatcher advances. Defaults to
// [DefaultCursorName]. Use it only if a service runs more than one logical cursor over
// the same outbox (uncommon — the SDK assumes one dispatcher per service).
func WithCursorName(name string) DispatcherOption {
	return func(d *Dispatcher) {
		if name != "" {
			d.cursorName = name
		}
	}
}

// NewDispatcher returns a Dispatcher that reads store forward via the cursors sidecar,
// runs handlers in tx, and dedups via idem. cursors is the
// [persistence.OutboxCursorStore] the dispatcher advances (its own progress — the
// outbox is write-only); pass nil to use a fresh in-memory
// [persistence.MemoryOutboxCursorStore] (dev default). tx is the
// [persistence.TxRunner] for the aggregates the handlers write (each handler runs
// inside it). idem may be nil to use a fresh in-memory [MemoryIdempotencyStore]. The
// poison cutoff defaults to [persistence.DefaultMaxOutboxAttempts]; override it with
// [WithMaxAttempts].
func NewDispatcher(store persistence.OutboxStore, cursors persistence.OutboxCursorStore, tx persistence.TxRunner, idem IdempotencyStore, opts ...DispatcherOption) *Dispatcher {
	if cursors == nil {
		cursors = persistence.NewMemoryOutboxCursorStore()
	}
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
	d := &Dispatcher{
		store:       store,
		cursors:     cursors,
		cursorName:  DefaultCursorName,
		tx:          tx,
		idem:        idem,
		maxAttempts: persistence.DefaultMaxOutboxAttempts,
		handlers:    make(map[string][]registeredHandler),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Subscribe registers fn as a handler for events of eventType (F032 G-4). name
// identifies the handler in the idempotency key so two handlers of the same event
// dedup independently. Multiple handlers may subscribe to one type; all must
// succeed for the event to be marked delivered.
func (d *Dispatcher) Subscribe(eventType, name string, fn Handler) {
	// Fail fast at registration (a setup-time call) rather than letting a nil handler
	// nil-panic on first delivery — which happens inside the poller goroutine, rolls
	// back, and re-panics up through Poll, silently crashing delivery without ever
	// reaching onErr. A nil handler is a programming error; surface it at the call site.
	if fn == nil {
		panic("events: Dispatcher.Subscribe called with a nil handler for event type " + eventType)
	}
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
//
// The separator is U+001F (ASCII Unit Separator), NOT a NUL byte: PostgreSQL
// rejects a NUL inside a text/varchar value ("invalid byte sequence for encoding
// UTF8: 0x00", SQLSTATE 22021), so a NUL-delimited key cannot be stored in the
// idempotency marker table on the production engine — even though SQLite tolerates
// it. U+001F is a non-printing control char that storage engines store as ordinary
// text and that does not appear in event ids (UUIDs) or handler names.
func idempotencyKey(eventID, handlerName string) string {
	return eventID + "\x1f" + handlerName
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

// RunOnce reads up to limit events forward from the sidecar cursor and delivers each
// in (created_time, id) order, advancing the cursor PAST every event it resolves. It
// returns the number of events resolved this pass (delivered, deduped, or
// dead-lettered) — i.e. how far the cursor advanced. A poller calls RunOnce on a tick;
// tests call it directly to drive delivery deterministically.
//
// F033 forward cursor (write-only outbox): the dispatcher NEVER mutates an outbox row.
// It loads its position from the sidecar, ReadAfter(cursor, limit) pulls the next batch
// without touching the rows, and on a successful deliver() it advances the sidecar
// cursor to that event. Because events are read in order, the FIRST event of the batch
// is the head (oldest un-consumed). If the head fails, the cursor cannot advance past
// it without skipping a gap, so the batch stops there — bounded head-of-line blocking.
// The head-failure count is tracked in the sidecar; after maxAttempts consecutive
// failures on the same head event it is dead-lettered (recorded in the sidecar) and the
// cursor advances past it so the stream is not wedged forever (AC-3). The outbox row is
// never touched — only the sidecar records the dead-letter verdict.
//
// Exactly-once effect: a crash between deliver() and the cursor-advance re-delivers the
// event on the next RunOnce; the per-(event, handler) idempotency marker (committed in
// the handler tx) makes that re-delivery a no-op — at-least-once + idempotency.
func (d *Dispatcher) RunOnce(ctx context.Context, limit int) (delivered int, err error) {
	cursor, headFailures, err := d.cursors.LoadCursor(ctx, d.cursorName)
	if err != nil {
		return 0, fmt.Errorf("load cursor %q: %w", d.cursorName, err)
	}
	batch, err := d.store.ReadAfter(ctx, cursor, limit)
	if err != nil {
		return 0, fmt.Errorf("read outbox after cursor: %w", err)
	}
	for _, rec := range batch {
		evt := eventFromRecord(rec)
		pos := persistence.OutboxCursor{CreatedTime: rec.CreatedTime, ID: rec.ID}
		if derr := d.deliver(ctx, evt); derr != nil {
			// The head event failed. Bump its sidecar head-failure count; do NOT advance
			// the cursor (advancing would skip the gap and lose the event — write-only
			// outbox, the cursor is the only progress). Stop the batch here so order and
			// the head-of-line property hold (a later event must not be delivered before
			// this one). After maxAttempts on this same head event, dead-letter it and
			// advance past it so a permanently failing event does not wedge the stream.
			headFailures++
			if headFailures >= d.maxAttempts {
				if dlErr := d.cursors.DeadLetter(ctx, d.cursorName, rec, derr.Error()); dlErr != nil {
					return delivered, fmt.Errorf("dead-letter %s: %w", rec.ID, dlErr)
				}
				if serr := d.cursors.SaveCursor(ctx, d.cursorName, pos, 0); serr != nil {
					return delivered, fmt.Errorf("advance past poison %s: %w", rec.ID, serr)
				}
				delivered++ // resolved by dead-lettering; the cursor moved past it
				return delivered, fmt.Errorf("dead-lettered poison event %s after %d attempts: %w", rec.ID, headFailures, derr)
			}
			if serr := d.cursors.SaveCursor(ctx, d.cursorName, cursor, headFailures); serr != nil {
				return delivered, fmt.Errorf("save head-failure count: %w", serr)
			}
			return delivered, fmt.Errorf("deliver %s: %w", rec.ID, derr)
		}
		// Delivered (every handler applied or was already-applied). Advance the cursor
		// past this event and reset the head-failure count — the next head is the next
		// event. The outbox row is untouched.
		cursor = pos
		headFailures = 0
		if serr := d.cursors.SaveCursor(ctx, d.cursorName, cursor, 0); serr != nil {
			return delivered, fmt.Errorf("advance cursor past %s: %w", rec.ID, serr)
		}
		delivered++
	}
	return delivered, nil
}

// Poll runs RunOnce on every tick of interval until ctx is cancelled. It is the
// in-process poller dev default; a caller wires it as a goroutine. Errors from a tick
// are reported through onErr (nil to ignore) so one bad event does not stop the loop.
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

// Cursor returns the dispatcher's current forward position (for tests/introspection):
// the (created_time, id) of the last event it consumed.
func (d *Dispatcher) Cursor(ctx context.Context) (persistence.OutboxCursor, error) {
	c, _, err := d.cursors.LoadCursor(ctx, d.cursorName)
	return c, err
}

// eventFromRecord rebuilds the Event a handler sees from an outbox row.
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
