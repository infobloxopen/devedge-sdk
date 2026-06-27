package events

import (
	"context"
	"errors"
)

// ErrBusClosed is returned by a [BusPublisher.Publish] after the bus has been closed,
// so the relay surfaces a clear shutdown error rather than blocking or panicking.
var ErrBusClosed = errors.New("events: bus closed")

// Bus is the backend-neutral event-bus seam that sits BETWEEN the write-only outbox
// and the registered handlers (Phase 1 of the event-bus stack). The full pipeline is:
//
//	aggregate tx → write-only outbox (Append) → RELAY (reads the outbox forward via
//	OutboxStore.ReadAfter + OutboxCursorStore, publishes each event to the bus) → BUS
//	(this seam) → CONSUMER (subscribes, runs each event through the registered handlers,
//	each in its own Atomically + IdempotencyStore) → handler ("propagation back in").
//
// The RELAY is the only producer onto the bus and the CONSUMER is the only path from
// the bus back to handlers — so the bus is the swappable transport in the middle. The
// dev default is an in-process channel bus ([github.com/infobloxopen/devedge-sdk/events/membus]);
// a Kafka (or other broker) bus drops in later behind this SAME interface.
//
// KAFKA-READY (Phase 2): the interface speaks in TOPIC and consumer-GROUP terms — the
// two concepts a broker needs — and leaves offsets, partitions, rebalancing, and
// at-least-once redelivery as the implementation's concern. A Kafka bus maps Publish to
// a producer send keyed by the message Key (so per-key/per-aggregate ordering is
// preserved on one partition) and Subscribe to a consumer-group subscription; the
// in-memory bus models the same contract in-process. NOTHING in this interface (or the
// in-memory impl) imports a broker/Kafka dependency — CLEAN CORE: the bus seam is pure
// Go and a broker adapter lives OUTSIDE this module.
//
// Delivery contract (the part handlers must rely on): the bus is AT-LEAST-ONCE. A bus
// may redeliver a message (a Kafka rebalance, a consumer crash before commit, the relay
// re-publishing after its own crash). The consumer dedups via the per-(event, handler)
// [IdempotencyStore] marker, so an at-least-once bus yields an exactly-once EFFECT. A
// bus implementation MUST therefore NOT silently drop a message it accepted, but MAY
// deliver it more than once.
type Bus interface {
	BusPublisher
	BusSubscriber
}

// BusPublisher is the relay's side of the bus: it puts an event onto a topic. It is a
// SEPARATE interface from the transactional-outbox [Publisher] (Publisher writes the
// outbox row inside the aggregate tx; BusPublisher hands an already-committed event to
// the transport). The relay holds a BusPublisher; a handler never does.
//
// NB the name: the older transactional-outbox seam in this package is [Publisher]
// (Publish(ctx, Event) into the outbox). To avoid a clash this bus-side producer is
// [BusPublisher]. The two are deliberately distinct — one is the outbox write, the
// other is the transport publish.
type BusPublisher interface {
	// Publish sends msg to topic. It returns once the bus has durably accepted the
	// message (for the in-memory bus, "durable" means enqueued for in-process
	// consumers; for Kafka, acked by the broker). topic groups related event types so
	// a consumer group can subscribe to exactly the streams it cares about. Returning
	// an error leaves the relay's cursor un-advanced so the relay re-publishes
	// (at-least-once) — the bus and downstream handlers must tolerate the redelivery.
	Publish(ctx context.Context, topic string, msg BusMessage) error
}

// BusSubscriber is the consumer's side of the bus: it registers a handler for a topic
// under a consumer GROUP, then a Consume loop pumps accepted messages to the registered
// handlers. Group is the at-least-once unit a broker load-balances and tracks offsets
// for; the in-memory bus models it as an independent delivery cursor per group so two
// different groups each see every message (fan-out) while members of one group share the
// stream.
type BusSubscriber interface {
	// Subscribe registers fn to receive messages published to topic, as a member of the
	// consumer group. Multiple Subscribe calls for the SAME (group, topic) add handlers
	// that all see every message of that topic (the consumer wires each registered
	// event handler this way). Distinct groups each get their own copy of the stream
	// (broker fan-out). Subscribe is a setup-time call made before Consume; it does not
	// itself deliver anything.
	Subscribe(group, topic string, fn BusHandler) error

	// Consume blocks pumping accepted messages to the handlers registered via Subscribe,
	// until ctx is cancelled, then returns ctx.Err() (or nil). A handler returning an
	// error signals the bus the message was NOT processed: the bus MUST redeliver it
	// (at-least-once) rather than advancing past it — the consumer relies on this so a
	// transient handler failure is retried and the per-(event, handler) idempotency
	// marker keeps the effect exactly-once. A bus that cannot redeliver in-process (the
	// message is gone) must at least not advance its delivery position past an
	// un-acked message.
	Consume(ctx context.Context) error
}

// Subscriber is the consumer-facing alias kept for the [Bus] composition and for
// callers that only need the subscribe/consume half.
type Subscriber = BusSubscriber

// BusHandler processes one message delivered off the bus. It returns nil to ACK the
// message (the bus may advance past it) or an error to NACK it (the bus must redeliver).
// The consumer wraps each registered domain [Handler] in a BusHandler that runs the
// handler in its own Atomically with the idempotency marker.
type BusHandler func(ctx context.Context, msg BusMessage) error

// BusMessage is the envelope carried on the bus. It wraps the domain [Event] with the
// transport key the bus uses for ordering/partitioning.
//
// Key is the partition/ordering key (Kafka-ready): a broker hashes Key to a partition
// and preserves order WITHIN a key. The relay sets Key to the event's AggregateID (with
// AggregateType as a fallback) so all events of one aggregate land on one partition and
// keep their commit order — the same per-aggregate ordering the forward-cursor outbox
// read already guarantees. The in-memory bus does not partition, but carries Key so the
// envelope is identical across transports.
type BusMessage struct {
	// Key is the partition/ordering key. Empty is allowed (no ordering guarantee for
	// keyless messages); the relay fills it from the event's aggregate identity.
	Key string
	// Event is the domain event being transported. Event.ID remains the idempotency
	// key the consumer dedups on, exactly as the in-process dispatcher did.
	Event Event
}

// busTopicForEvent is the default topic an event is published to when the relay is not
// given an explicit topic mapping: the event's Type. Keeping topic == event type is the
// simplest Kafka-ready default (one topic per event type); a service may override it
// with a relay topic mapper.
func busTopicForEvent(evt Event) string { return evt.Type }

// busKeyForEvent is the default partition/ordering key for an event: its AggregateID,
// falling back to AggregateType, so per-aggregate order is preserved on a partitioned
// broker. Empty when the event references no aggregate (keyless).
func busKeyForEvent(evt Event) string {
	if evt.AggregateID != "" {
		return evt.AggregateID
	}
	return evt.AggregateType
}
