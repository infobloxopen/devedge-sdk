package events

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// Handler reacts to a delivered domain event by changing ANOTHER aggregate. The
// [Consumer] runs each handler in its own [persistence.TxRunner.Atomically] (see
// [Consumer.deliver]), so a handler should treat ctx as already transactional and do
// a single-aggregate write — the reaction is itself a safe aggregate write (F032
// G-4). Returning an error leaves the event undelivered so it is re-tried
// (at-least-once); handlers MUST therefore be idempotent (F032 D-4).
type Handler func(ctx context.Context, evt Event) error

// ErrAlreadyApplied is returned by [IdempotencyStore.Record] when key has already
// been recorded. It is the exactly-once guard: Record is called INSIDE the
// handler's transaction, so a concurrent (or redelivered) double-delivery races to
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
// guarantee). The [Consumer] enrolls it automatically for a MemoryTxRunner.
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

// DefaultCursorName is the sidecar cursor name a [Relay] (and the compatibility
// [Dispatcher]) uses when the caller does not set one with [WithCursorName] /
// [WithRelayCursorName]. A service runs a single relay instance (the SDK assumption — the
// external WAL/queue consumer is the scale-out path), so one named cursor per relay is
// sufficient.
const DefaultCursorName = "default"

// registeredHandler pairs a handler with the name it dedups under.
type registeredHandler struct {
	name string
	fn   Handler
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

// eventFromRecord rebuilds the Event a handler sees from an outbox row. The
// per-tenant EventSeq and EventEpoch ride along so a consumer can order/dedup a
// tenant's events by (AccountID, EventSeq) and fence a superseded epoch after a
// tenant move (cell-based development).
func eventFromRecord(rec *persistence.OutboxRecord) Event {
	return Event{
		ID:            rec.ID,
		Type:          rec.EventType,
		AggregateType: rec.AggregateType,
		AggregateID:   rec.AggregateID,
		AccountID:     rec.AccountID,
		Payload:       rec.Payload,
		EventSeq:      rec.EventSeq,
		EventEpoch:    rec.EventEpoch,
	}
}

// Dispatcher is the SINGLE-PROCESS compatibility façade that composes the event-bus
// stack — [Relay] (outbox→bus) + a synchronous in-process bus + [Consumer] (bus→handlers)
// — behind the stable, pre-bus dispatcher API. It exists so callers (and the SDK's own
// tests/examples) that wired the in-process forward-cursor dispatcher keep working while
// the stack underneath is the new relay/bus/consumer split.
//
// It delivers committed outbox events to registered handlers at-least-once (F032 G-3) for
// SAME-DB cross-aggregate reactions (e.g. iam UserSuspended → revoke the user's API keys).
// Under the hood [Dispatcher.RunOnce] pumps the relay one batch onto a synchronous bus
// that delivers each message straight to the consumer's transactional, idempotency-guarded
// handler path and propagates the handler result back — so the relay's forward cursor,
// head-of-line poison handling (dead-letter after maxAttempts, then advance), and
// write-only invariant are preserved exactly as before, now expressed as relay+consumer.
//
// For the full asynchronous stack (a real bus, a relay poll loop, and a consumer Consume
// loop running as separate goroutines, Kafka-ready) wire [NewRelay] + a [Bus] (e.g.
// [github.com/infobloxopen/devedge-sdk/events/membus]) + [NewConsumer] directly. The
// Dispatcher is the synchronous, single-process convenience over the same machinery.
//
// Exactly-once effect: delivery is AT-LEAST-ONCE (a crash between deliver and
// cursor-advance, or a redelivered message), guarded into an exactly-once EFFECT by the
// per-(event, handler) [IdempotencyStore] marker, which commits INSIDE the handler's
// transaction.
type Dispatcher struct {
	relay    *Relay
	consumer *Consumer
	bus      *syncBus

	mu          sync.Mutex
	maxAttempts int
}

// DispatcherOption configures a Dispatcher.
type DispatcherOption func(*dispatcherConfig)

type dispatcherConfig struct {
	cursorName  string
	maxAttempts int
}

// WithMaxAttempts sets the poison cutoff: after this many consecutive failed
// deliveries of the SAME head event, the dispatcher dead-letters it (in the sidecar)
// and advances the cursor past it (F033). A non-positive value keeps the default
// ([persistence.DefaultMaxOutboxAttempts]).
func WithMaxAttempts(n int) DispatcherOption {
	return func(c *dispatcherConfig) {
		if n > 0 {
			c.maxAttempts = n
		}
	}
}

// WithCursorName sets the sidecar cursor name this dispatcher advances. Defaults to
// [DefaultCursorName]. Use it only if a service runs more than one logical cursor over
// the same outbox (uncommon — the SDK assumes one dispatcher per service).
func WithCursorName(name string) DispatcherOption {
	return func(c *dispatcherConfig) {
		if name != "" {
			c.cursorName = name
		}
	}
}

// NewDispatcher returns the compatibility Dispatcher composing a relay, a synchronous
// in-process bus, and a consumer. store is read forward via the cursors sidecar
// (pass nil for a fresh in-memory [persistence.MemoryOutboxCursorStore]); tx is the
// [persistence.TxRunner] the handlers write in; idem may be nil for a fresh in-memory
// [MemoryIdempotencyStore]. The poison cutoff defaults to
// [persistence.DefaultMaxOutboxAttempts]; override it with [WithMaxAttempts].
func NewDispatcher(store persistence.OutboxStore, cursors persistence.OutboxCursorStore, tx persistence.TxRunner, idem IdempotencyStore, opts ...DispatcherOption) *Dispatcher {
	cfg := &dispatcherConfig{cursorName: DefaultCursorName, maxAttempts: persistence.DefaultMaxOutboxAttempts}
	for _, opt := range opts {
		opt(cfg)
	}
	if cursors == nil {
		cursors = persistence.NewMemoryOutboxCursorStore()
	}
	bus := newSyncBus()
	d := &Dispatcher{
		bus:         bus,
		consumer:    NewConsumer(bus, tx, idem),
		maxAttempts: cfg.maxAttempts,
	}
	// The relay publishes the outbox onto the synchronous bus; the consumer is the bus's
	// only subscriber. The relay's maxAttempts is what the dispatcher's poison test asserts
	// on, but in the synchronous façade a handler failure surfaces through the bus Publish,
	// so the relay's head-of-line/dead-letter machinery is what drives the poison behaviour.
	d.relay = NewRelay(store, cursors, bus,
		WithRelayCursorName(cfg.cursorName),
		WithRelayMaxAttempts(cfg.maxAttempts),
	)
	return d
}

// Subscribe registers fn as a handler for events of eventType (F032 G-4). name
// identifies the handler in the idempotency key so two handlers of the same event
// dedup independently. Multiple handlers may subscribe to one type; all must
// succeed for the event to be marked delivered.
func (d *Dispatcher) Subscribe(eventType, name string, fn Handler) {
	d.consumer.Subscribe(eventType, name, fn)
}

// RunOnce reads up to limit events forward from the sidecar cursor and delivers each
// in (created_time, id) order, advancing the cursor PAST every event it resolves. It
// returns the number of events resolved this pass (delivered, deduped, or
// dead-lettered) — i.e. how far the cursor advanced. A poller calls RunOnce on a tick;
// tests call it directly to drive delivery deterministically.
//
// It is a synchronous pump of the relay onto the in-process bus: the relay reads the next
// batch, publishes each event to the synchronous bus, and the bus delivers it straight
// through the consumer's transactional idempotency-guarded handler path. A handler error
// becomes a bus-Publish error, so the relay leaves the cursor un-advanced (at-least-once)
// and, after maxAttempts on the same head event, dead-letters it and advances past it —
// preserving the pre-bus dispatcher semantics exactly. The write-only outbox is never
// mutated.
func (d *Dispatcher) RunOnce(ctx context.Context, limit int) (delivered int, err error) {
	// Serialize concurrent RunOnce on ONE dispatcher so two goroutines do not both pump
	// the relay against the same cursor at once. (The cross-dispatcher concurrent test
	// deliberately uses two SEPARATE dispatchers sharing a cursor to prove the idempotency
	// marker — not this lock — is the exactly-once guard.)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.relay.PumpOnce(ctx, limit)
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
	return d.relay.Cursor(ctx)
}

// syncBus is the SYNCHRONOUS, in-process bus the compatibility [Dispatcher] uses: a
// Publish delivers the message straight to the registered handlers in the calling
// goroutine and returns their result, so the relay's PumpOnce sees a handler failure as a
// Publish error and applies its head-of-line/poison machinery. It is NOT the async
// dev/embedded bus (that is package membus); it exists only to make the relay+consumer
// stack behave like the old single-call dispatcher.
type syncBus struct {
	mu   sync.RWMutex
	subs map[string][]BusHandler // topic -> handlers (one per group key, but the dispatcher uses one group)
}

func newSyncBus() *syncBus {
	return &syncBus{subs: make(map[string][]BusHandler)}
}

// Subscribe implements [BusSubscriber]: register fn for topic. The group is ignored — the
// synchronous bus has a single in-process consumer (the dispatcher's consumer).
func (b *syncBus) Subscribe(group, topic string, fn BusHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], fn)
	return nil
}

// Publish implements [BusPublisher]: deliver msg synchronously to every handler for the
// topic, returning the first handler error so the relay treats it as an un-published head
// event (cursor stays put → at-least-once → poison after maxAttempts).
func (b *syncBus) Publish(ctx context.Context, topic string, msg BusMessage) error {
	b.mu.RLock()
	hs := append([]BusHandler(nil), b.subs[topic]...)
	b.mu.RUnlock()
	for _, fn := range hs {
		if err := fn(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

// Consume implements [BusSubscriber]: the synchronous bus delivers on Publish, so Consume
// has nothing to pump — it simply blocks until ctx is cancelled. The Dispatcher drives
// delivery through RunOnce/Poll, not Consume.
func (b *syncBus) Consume(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// compile-time check.
var _ Bus = (*syncBus)(nil)
