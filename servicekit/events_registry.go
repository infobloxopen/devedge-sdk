package servicekit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/events/membus"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// hostEventRegistry is the WS-012 P3 EventRegistry: the HOST owns the dispatcher
// lifecycle for the whole composition. It collects each module's outbox + handler
// registrations during Register, then [Run] starts exactly ONE relay + ONE consumer
// per module outbox over the ONE shared [events.Bus]. This is what makes "same
// binary != direct calls" true: a module that reacts to another module's event does
// so by SUBSCRIBING on the shared bus, never by importing the other module's
// handler. A composed binary that naively started a relay+consumer per service's
// main() would double-start dispatchers; the host owns them so it starts each
// exactly once.
//
// Same-binary, one-DB → the shared bus is in-process ([membus]); the abstraction is
// still a durable outbox → relay → bus → consumer pipeline (NOT a direct handler
// call). Across DBs/daemons the same registrations drive a Kafka bus behind the
// SAME [events.Bus] seam (P4/deploy concern).
type hostEventRegistry struct {
	bus events.Bus // the ONE shared bus for the composition (in-process by default)

	mu sync.Mutex
	// outboxes is the per-module relay registration; one per module that declares a
	// transactional outbox. The host starts exactly one relay per entry.
	outboxes []outboxRegistration
	// consumers is the per-module consumer registration (its handlers); one per
	// module that subscribes to any event. The host starts exactly one consumer per
	// entry, each in its OWN consumer group so every module sees every event it
	// subscribes to (broker fan-out), even two modules subscribing to the same type.
	consumers []consumerRegistration
}

// OutboxRelayConfig is the per-module outbox a module hands the host so the host can
// own its relay (outbox → shared bus). The module supplies its NAMESPACED stores
// (built from its [DatabaseNamespace] per P2) — the host never reaches into a
// module's database, it only drives the relay loop. A module registers this from its
// Register via [EventRegistry.RegisterOutbox].
type OutboxRelayConfig struct {
	// Store is the module's (namespaced) write-only outbox the relay reads forward.
	Store persistence.OutboxStore
	// Cursors is the module's (namespaced) sidecar cursor store the relay advances.
	// Each module's relay has its OWN cursor (P2 namespaced it per module), so two
	// co-resident relays never share a position. nil uses a fresh in-memory cursor.
	Cursors persistence.OutboxCursorStore
	// Leader gates the relay so only one replica pumps the module's outbox. nil uses
	// a single-process leader (dev/single-replica). A composed host passes a
	// cross-process leader (PG advisory lock) per module for multi-replica.
	Leader events.Leader
	// PollInterval is how often the relay pumps. Zero defaults to one second.
	PollInterval time.Duration
	// Batch is the max events pumped per tick. Zero defaults to 100.
	Batch int
}

// ConsumerConfig is the per-module consumer a module hands the host so the host can
// own its consumer (shared bus → the module's handlers). The module supplies the tx
// runner + idempotency store its handlers commit through (built from its namespaced
// DB per P2); the host runs the consumer loop in the module's bulkhead.
type ConsumerConfig struct {
	// Tx is the [persistence.TxRunner] each handler's reaction + idempotency marker
	// commit through (the exactly-once guard). Required when registering a handler.
	Tx persistence.TxRunner
	// Idem is the module's (namespaced) idempotency store. nil uses an in-memory
	// store enrolled in the module's memory tx runner (dev default).
	Idem events.IdempotencyStore
}

// EventHandler is one subscription a module registers: the event type, a stable
// handler name (the idempotency-key discriminator), and the reaction. The host
// wires it onto the module's consumer.
type EventHandler struct {
	// EventType is the globally-unique event type the handler reacts to (it must
	// match a Subscribes entry in the module's Descriptor, validated at boot).
	EventType string
	// Name identifies the handler in the per-(event, handler) idempotency key so two
	// handlers of one event dedup independently. Module-qualify it.
	Name string
	// Handle is the reaction; the host runs it inside ConsumerConfig.Tx with the
	// idempotency marker.
	Handle events.Handler
}

// outboxRegistration is the resolved per-module relay the host will start.
type outboxRegistration struct {
	moduleID string
	cfg      OutboxRelayConfig
}

// consumerRegistration is the resolved per-module consumer the host will start.
type consumerRegistration struct {
	moduleID string
	cfg      ConsumerConfig
	handlers []EventHandler
}

// newHostEventRegistry builds the registry over the host's shared bus. A nil bus
// defaults to an in-process membus (same-binary, one-DB composition) — the dev/
// single-binary default the proposal calls for.
func newHostEventRegistry(bus events.Bus) *hostEventRegistry {
	if bus == nil {
		bus = membus.New()
	}
	return &hostEventRegistry{bus: bus}
}

// RegisterOutbox records the module's outbox so the host starts exactly one relay for
// it (proposal §5.5). It is the P3 replacement for the inert P1 stub: the
// OutboxDescriptor's Enabled flag still gates whether the host runs a relay, and the
// module's namespaced stores arrive via [App.RegisterOutboxRelay]. This descriptor-
// only form keeps the [EventRegistry] interface backward-compatible; the relay stores
// flow through the App helper so the interface stays minimal.
func (r *hostEventRegistry) RegisterOutbox(moduleID string, d OutboxDescriptor) error {
	// The descriptor only declares intent; the namespaced stores arrive via
	// registerRelay. A disabled outbox needs no relay.
	if !d.Enabled {
		return nil
	}
	return nil
}

// registerRelay records a module's namespaced relay stores so the host starts one
// relay per module outbox. Called from [App.RegisterOutboxRelay] in a module's
// Register.
func (r *hostEventRegistry) registerRelay(moduleID string, cfg OutboxRelayConfig) error {
	if cfg.Store == nil {
		return fmt.Errorf("servicekit: module %q RegisterOutboxRelay: Store is nil", moduleID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.outboxes {
		if o.moduleID == moduleID {
			// One relay per module outbox — the whole point (no double-start).
			return fmt.Errorf("servicekit: module %q already registered an outbox relay (one per module)", moduleID)
		}
	}
	r.outboxes = append(r.outboxes, outboxRegistration{moduleID: moduleID, cfg: cfg})
	return nil
}

// registerConsumer records a module's handlers so the host starts one consumer per
// module. Called from [App.Subscribe] in a module's Register.
func (r *hostEventRegistry) registerConsumer(moduleID string, cfg ConsumerConfig, handlers ...EventHandler) error {
	if len(handlers) == 0 {
		return nil
	}
	if cfg.Tx == nil {
		return fmt.Errorf("servicekit: module %q Subscribe: ConsumerConfig.Tx is nil (handlers need a tx runner)", moduleID)
	}
	for _, h := range handlers {
		if h.EventType == "" || h.Handle == nil {
			return fmt.Errorf("servicekit: module %q Subscribe: handler %q has empty EventType or nil Handle", moduleID, h.Name)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consumers = append(r.consumers, consumerRegistration{moduleID: moduleID, cfg: cfg, handlers: handlers})
	return nil
}

// dispatchers builds the relays + consumers from the recorded registrations: exactly
// one relay per module outbox (each over the module's own namespaced cursor) and one
// consumer per module (each in its own consumer group). It returns them un-started;
// [Run] starts each under the module's bulkhead. Building (not starting) here keeps
// the start ordering — and the "exactly one per module" assertion — in Run.
func (r *hostEventRegistry) dispatchers() ([]*moduleRelay, []*moduleConsumer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	relays := make([]*moduleRelay, 0, len(r.outboxes))
	for _, o := range r.outboxes {
		opts := []events.RelayOption{}
		if o.cfg.Leader != nil {
			opts = append(opts, events.WithRelayLeader(o.cfg.Leader))
		}
		relay := events.NewRelay(o.cfg.Store, o.cfg.Cursors, r.bus, opts...)
		interval := o.cfg.PollInterval
		if interval <= 0 {
			interval = time.Second
		}
		batch := o.cfg.Batch
		if batch <= 0 {
			batch = 100
		}
		relays = append(relays, &moduleRelay{moduleID: o.moduleID, relay: relay, interval: interval, batch: batch})
	}

	consumers := make([]*moduleConsumer, 0, len(r.consumers))
	for _, c := range r.consumers {
		// One consumer group PER MODULE so every module sees every event it
		// subscribes to (broker fan-out): two modules subscribing to the same type
		// each get their own copy, rather than load-balancing within one group.
		cons := events.NewConsumer(r.bus, c.cfg.Tx, c.cfg.Idem, events.WithConsumerGroup("module:"+c.moduleID))
		for _, h := range c.handlers {
			cons.Subscribe(h.EventType, h.Name, h.Handle)
		}
		consumers = append(consumers, &moduleConsumer{moduleID: c.moduleID, consumer: cons})
	}
	return relays, consumers
}

// moduleRelay is one module's outbox→bus relay plus its poll settings.
type moduleRelay struct {
	moduleID string
	relay    *events.Relay
	interval time.Duration
	batch    int
}

// moduleConsumer is one module's bus→handlers consumer.
type moduleConsumer struct {
	moduleID string
	consumer *events.Consumer
}

// Bus exposes the shared bus so a module's Register can build its own publisher over
// it when needed (rare — most publishing goes through the outbox). Kept unexported on
// the type and surfaced via App where it is the documented seam.
func (r *hostEventRegistry) sharedBus() events.Bus { return r.bus }

// closeBus closes the shared bus if it is closeable, so a clean host shutdown stops
// in-flight deliveries (membus). It is a no-op for buses without a Close.
func (r *hostEventRegistry) closeBus() {
	if c, ok := r.bus.(interface{ Close() }); ok {
		c.Close()
	}
}

// startDispatchers starts every module's relay + consumer under its bulkhead and
// returns once they are running. Each relay/consumer runs in its own supervised
// goroutine keyed by the module ID, so a panic in one module's dispatcher is
// contained and attributed to that module (it does not crash the host or another
// module). ctx cancellation stops them all.
func (r *hostEventRegistry) startDispatchers(ctx context.Context, sup *supervisor) {
	relays, consumers := r.dispatchers()
	for _, mr := range relays {
		mr := mr
		sup.Go(mr.moduleID, "relay", func() {
			mr.relay.Run(ctx, mr.interval, mr.batch, func(err error) {
				// A pump error caused by host shutdown (ctx cancelled) is benign — the
				// relay re-publishes from the outbox on the next boot (at-least-once); do
				// not attribute it to the module as a failure.
				if ctx.Err() != nil {
					return
				}
				sup.reportError(mr.moduleID, fmt.Errorf("relay: %w", err))
			})
		})
	}
	for _, mc := range consumers {
		mc := mc
		sup.Go(mc.moduleID, "consumer", func() {
			if err := mc.consumer.Run(ctx); err != nil && ctx.Err() == nil {
				sup.reportError(mc.moduleID, fmt.Errorf("consumer: %w", err))
			}
		})
	}
}
