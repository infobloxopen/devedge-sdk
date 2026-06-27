package events

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// DefaultConsumerGroup is the bus consumer group a [Consumer] subscribes under when the
// caller does not set one with [WithConsumerGroup]. A service runs one consumer that
// propagates outbox events to its handlers, so one group is sufficient; a broker fans the
// stream out to additional groups (other services / read models) independently.
const DefaultConsumerGroup = "default"

// Consumer is the bus→handlers pump of the event-bus stack (Phase 1) — the "propagation
// back in". It SUBSCRIBES to the [Bus] (which the [Relay] publishes the outbox to) and,
// for each delivered event, runs every registered domain [Handler] in its OWN
// [persistence.TxRunner.Atomically] transaction with the per-(event, handler)
// [IdempotencyStore] marker, so the cross-aggregate reaction is a safe single-aggregate
// write reached by eventual consistency and applied EXACTLY ONCE under an at-least-once
// bus.
//
// This is the dispatch half of the old in-process dispatcher's loop, lifted onto the
// other side of the bus seam: the handler-registration API (Subscribe by event type), the
// idempotency-guarded transactional deliver, and the tenant-scoping of the handler are
// unchanged — only the trigger moved from "the dispatcher read the outbox" to "the bus
// delivered a message the relay published".
//
// Exactly-once effect: the bus is AT-LEAST-ONCE (a redelivery on a consumer crash, a
// broker rebalance, or the relay re-publishing). Each handler's effect is guarded into
// exactly-once by the unique idempotency marker committed INSIDE the handler's
// transaction — a redelivery whose marker is already committed is a no-op. A handler error
// NACKs the message so the bus redelivers it (at-least-once), and the marker keeps the
// effect at one across the retry. Handlers MUST be idempotent.
type Consumer struct {
	bus   BusSubscriber
	group string
	tx    persistence.TxRunner
	idem  IdempotencyStore

	mu       sync.RWMutex
	handlers map[string][]registeredHandler
	wired    map[string]struct{} // event types already Subscribe()d on the bus
}

// ConsumerOption configures a [Consumer].
type ConsumerOption func(*Consumer)

// WithConsumerGroup sets the bus consumer group this consumer subscribes under. Defaults
// to [DefaultConsumerGroup]. Use distinct groups to run independent consumers that each
// see every event (broker fan-out).
func WithConsumerGroup(group string) ConsumerOption {
	return func(c *Consumer) {
		if group != "" {
			c.group = group
		}
	}
}

// NewConsumer returns a Consumer that subscribes to bus, runs handlers in tx, and dedups
// via idem. tx is the [persistence.TxRunner] for the aggregates the handlers write (each
// handler runs inside it). idem may be nil to use a fresh in-memory
// [MemoryIdempotencyStore] (dev default); for the in-memory store the consumer enrolls its
// marker repo in a [persistence.MemoryTxRunner] automatically so the marker commits in the
// handler's tx (the exactly-once guarantee).
func NewConsumer(bus BusSubscriber, tx persistence.TxRunner, idem IdempotencyStore, opts ...ConsumerOption) *Consumer {
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
	c := &Consumer{
		bus:      bus,
		group:    DefaultConsumerGroup,
		tx:       tx,
		idem:     idem,
		handlers: make(map[string][]registeredHandler),
		wired:    make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Subscribe registers fn as a handler for events of eventType and wires the consumer to
// the bus topic for that type (the relay's default topic IS the event type). name
// identifies the handler in the idempotency key so two handlers of the same event dedup
// independently. Multiple handlers may subscribe to one type; all must succeed (ACK) for
// the message to be considered delivered — a NACK from any one redelivers the whole
// message (at-least-once), and the idempotency marker skips the already-applied handlers.
//
// It is a setup-time call: make all Subscribe calls before [Consumer.Run]/the bus
// Consume loop starts.
func (c *Consumer) Subscribe(eventType, name string, fn Handler) {
	// Fail fast at registration rather than nil-panicking inside the consume loop.
	if fn == nil {
		panic("events: Consumer.Subscribe called with a nil handler for event type " + eventType)
	}
	c.mu.Lock()
	c.handlers[eventType] = append(c.handlers[eventType], registeredHandler{name: name, fn: fn})
	_, alreadyWired := c.wired[eventType]
	if !alreadyWired {
		c.wired[eventType] = struct{}{}
	}
	c.mu.Unlock()

	// Wire ONE bus subscription per event type (topic); the bus handler fans to all
	// registered domain handlers for the type via deliver(). Subscribing once per type
	// (not once per handler) avoids re-delivering the message to deliver() N times.
	if !alreadyWired {
		if err := c.bus.Subscribe(c.group, eventType, c.busHandler); err != nil {
			// Subscribe is a setup-time call; a bus that rejects a subscription is a
			// misconfiguration that should surface loudly rather than silently dropping
			// the type's deliveries.
			panic(fmt.Sprintf("events: Consumer bus.Subscribe(group=%q, topic=%q): %v", c.group, eventType, err))
		}
	}
}

// busHandler is the single [BusHandler] registered per topic. It receives a bus message
// and delivers its event to every domain handler registered for the event type, returning
// an error (NACK) if any handler fails so the bus redelivers (at-least-once).
func (c *Consumer) busHandler(ctx context.Context, msg BusMessage) error {
	return c.deliver(ctx, msg.Event)
}

func (c *Consumer) handlersFor(eventType string) []registeredHandler {
	c.mu.RLock()
	defer c.mu.RUnlock()
	hs := c.handlers[eventType]
	out := make([]registeredHandler, len(hs))
	copy(out, hs)
	return out
}

// deliver runs every handler subscribed to evt.Type. For each handler it runs the
// reaction AND records the idempotency marker in ONE transaction, so the effect and the
// marker commit or roll back together. The exactly-once guard is the in-tx unique marker:
// on a concurrent or redelivered double-delivery, both copies race to record the same
// (event, handler) marker; exactly one commits (effect + marker) and the other gets
// [ErrAlreadyApplied] and rolls its whole transaction back, so the side effect runs once
// even though the event was delivered twice. [IdempotencyStore.Seen] is only a fast-path
// pre-check; correctness does NOT depend on it.
//
// deliver returns nil only when every handler is applied (committed now or previously), so
// the bus ACKs the message only once all reactions are durable; any error NACKs it for
// redelivery.
func (c *Consumer) deliver(ctx context.Context, evt Event) error {
	// Scope the handler to the EVENT's tenant. The relay/bus carry events across all
	// tenants (the outbox has no TenantMixin), so the consume ctx's tenant is unrelated
	// to the event's. A tenant-scoped repository reads middleware.TenantIDFromContext to
	// filter its writes; running the handler on the consumer's (or no) tenant would let
	// it read/write ANOTHER tenant's aggregates — a cross-tenant write. Inject the event's
	// account so the reaction targets exactly the tenant that emitted it.
	if evt.AccountID != "" {
		ctx = middleware.WithTenantID(ctx, evt.AccountID)
	}
	for _, h := range c.handlersFor(evt.Type) {
		key := idempotencyKey(evt.ID, h.name)
		// Fast path: skip a handler whose marker is already committed.
		seen, err := c.idem.Seen(ctx, key)
		if err != nil {
			return fmt.Errorf("idempotency check: %w", err)
		}
		if seen {
			continue // already applied — redelivery no-op
		}
		// Run the reaction AND record the marker in ONE aggregate transaction: the marker
		// is unique, so a concurrent double-apply collides and the loser's whole tx
		// (effect + marker) rolls back — the side effect commits exactly once.
		txErr := c.tx.Atomically(ctx, func(ctx context.Context) error {
			if herr := h.fn(ctx, evt); herr != nil {
				return herr
			}
			return c.idem.Record(ctx, key)
		})
		if txErr != nil {
			if errors.Is(txErr, ErrAlreadyApplied) {
				// A concurrent (or earlier) delivery already committed this
				// (event, handler); our transaction rolled the duplicate effect back.
				// The handler IS applied — treat as a no-op success and move on.
				continue
			}
			return fmt.Errorf("handler %q for %q: %w", h.name, evt.Type, txErr)
		}
	}
	return nil
}

// Run blocks pumping the bus to the registered handlers until ctx is cancelled, then
// returns the bus's Consume error (ctx.Err() on cancellation). It is a thin pass-through
// to the bus Consume loop, kept so a service wires the consumer symmetrically with the
// relay: `go relay.Run(...)` and `go consumer.Run(...)`. All Subscribe calls must be made
// before Run.
func (c *Consumer) Run(ctx context.Context) error {
	return c.bus.Consume(ctx)
}
