// Package membus is the in-process, channel-based dev/embedded implementation of the
// [events.Bus] seam — Phase 1 of the SDK event-bus stack. It carries events from the
// relay (which reads the write-only outbox) to the consumer (which propagates them to
// the registered handlers), entirely in one process.
//
// CLEAN CORE: this package is pure Go. It imports NO broker/Kafka dependency — only the
// standard library and the backend-neutral [events] seam. A Kafka (or other broker) bus
// implements the SAME [events.Bus] interface OUTSIDE the core; nothing here leaks a
// transport dependency into the SDK.
//
// SCOPE (Phase 1, in-memory): single-process Publish→Subscribe. Multi-replica delivery
// (a message published on replica A reaching a consumer on replica B) is OUT OF SCOPE —
// that is what the Kafka bus (Phase 2) is for. In a single process the bus is reliable
// in-memory; on process exit, undelivered in-channel messages are lost, which is safe
// because the RELAY only advances its outbox cursor AFTER the bus accepts the message,
// so a restart re-publishes from the outbox (at-least-once). The bus models a broker's
// at-least-once contract: a handler that NACKs (returns an error) gets the message
// redelivered rather than dropped.
package membus

import (
	"context"
	"sync"

	"github.com/infobloxopen/devedge-sdk/events"
)

// DefaultBuffer is the per-(group,topic) channel buffer used when [New] is called
// without [WithBuffer]. It bounds in-flight messages so a slow consumer applies
// backpressure to the relay (Publish blocks) rather than growing memory without bound.
const DefaultBuffer = 256

// Bus is the in-memory [events.Bus]: channel-based, in-process Publish→Subscribe with
// per-consumer-group fan-out and at-least-once redelivery on handler NACK.
//
// Fan-out model: each distinct consumer GROUP gets its OWN delivery of every message on
// a topic (broker fan-out across groups). Within a group, all handlers registered for a
// topic see every message of that topic (the consumer registers each domain handler as a
// member of one group). Per-key ordering is preserved because a group's messages are
// delivered in publish order from a single FIFO channel and a NACK blocks the channel
// until the message is re-processed (so a later message never overtakes a failing one) —
// matching the bounded head-of-line behaviour of the forward-cursor outbox.
type Bus struct {
	buffer int

	mu     sync.Mutex
	groups map[string]*group // keyed by consumer group name
	closed bool
}

// group is one consumer group: a set of topic subscriptions sharing one delivery loop.
type group struct {
	name string
	buf  int

	mu   sync.Mutex
	subs map[string][]events.BusHandler // topic -> handlers
	ch   chan envelope                  // FIFO delivery channel for this group
}

// envelope pairs a delivered message with its topic so the consume loop dispatches it
// to the handlers registered for that topic.
type envelope struct {
	topic string
	msg   events.BusMessage
}

// Option configures a [Bus].
type Option func(*Bus)

// WithBuffer sets the per-group channel buffer (default [DefaultBuffer]). A larger
// buffer lets the relay get further ahead of a slow consumer; a smaller one applies
// backpressure sooner.
func WithBuffer(n int) Option {
	return func(b *Bus) {
		if n > 0 {
			b.buffer = n
		}
	}
}

// New returns an in-memory bus ready for Subscribe/Publish/Consume.
func New(opts ...Option) *Bus {
	b := &Bus{buffer: DefaultBuffer, groups: make(map[string]*group)}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

func (b *Bus) groupFor(name string) *group {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groups[name]
	if g == nil {
		g = &group{
			name: name,
			buf:  b.buffer,
			subs: make(map[string][]events.BusHandler),
			ch:   make(chan envelope, b.buffer),
		}
		b.groups[name] = g
	}
	return g
}

// Subscribe implements [events.BusSubscriber]: register fn for topic under group. All
// handlers of a (group, topic) see every message of that topic; distinct groups each get
// their own copy of the stream (fan-out). It is a setup-time call before Consume.
func (b *Bus) Subscribe(groupName, topic string, fn events.BusHandler) error {
	if fn == nil {
		// A nil handler would nil-panic inside the consume loop; fail fast at the
		// setup-time Subscribe call instead, exactly as the dispatcher's Subscribe does.
		panic("membus: Subscribe called with a nil handler for topic " + topic)
	}
	g := b.groupFor(groupName)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.subs[topic] = append(g.subs[topic], fn)
	return nil
}

// Publish implements [events.BusPublisher]: enqueue msg on topic for every consumer
// group that has at least one subscription (fan-out across groups). A group with no
// subscription for the topic still enqueues nothing for that topic (its consume loop
// simply finds no handler) — but we only enqueue to groups so a slow group's buffer
// bounds the relay. Publish blocks when a group's buffer is full (backpressure) or until
// ctx is cancelled.
func (b *Bus) Publish(ctx context.Context, topic string, msg events.BusMessage) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return events.ErrBusClosed
	}
	// Snapshot the current groups so we deliver to every group that exists at publish
	// time. (A group created later does not retroactively receive earlier messages —
	// the in-memory bus has no retained log; that is the Kafka bus's job.)
	gs := make([]*group, 0, len(b.groups))
	for _, g := range b.groups {
		gs = append(gs, g)
	}
	b.mu.Unlock()

	env := envelope{topic: topic, msg: msg}
	for _, g := range gs {
		// Only enqueue to a group that actually subscribes to this topic, so an
		// unrelated group's buffer is not consumed (and a group that ignores the topic
		// applies no backpressure for it).
		if !g.subscribes(topic) {
			continue
		}
		select {
		case g.ch <- env:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (g *group) subscribes(topic string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.subs[topic]) > 0
}

func (g *group) handlersFor(topic string) []events.BusHandler {
	g.mu.Lock()
	defer g.mu.Unlock()
	hs := g.subs[topic]
	out := make([]events.BusHandler, len(hs))
	copy(out, hs)
	return out
}

// Consume implements [events.BusSubscriber]: block delivering enqueued messages to the
// handlers registered via Subscribe, for ALL groups, until ctx is cancelled. It fans a
// goroutine per group so each group drains its own FIFO independently, then waits for
// cancellation. Returns ctx.Err() on cancellation.
//
// At-least-once: if a handler returns an error (NACK), the message is RE-DELIVERED to
// that handler — the loop retries it before pulling the next message, so order is held
// (a later message never overtakes a NACKing one) and a transient failure converges once
// the handler succeeds. The per-(event, handler) idempotency marker in the consumer keeps
// the EFFECT exactly-once across the redelivery.
func (b *Bus) Consume(ctx context.Context) error {
	b.mu.Lock()
	gs := make([]*group, 0, len(b.groups))
	for _, g := range b.groups {
		gs = append(gs, g)
	}
	b.mu.Unlock()

	var wg sync.WaitGroup
	for _, g := range gs {
		wg.Add(1)
		go func(g *group) {
			defer wg.Done()
			g.run(ctx)
		}(g)
	}
	<-ctx.Done()
	wg.Wait()
	return ctx.Err()
}

// run drains one group's channel, dispatching each message to the topic's handlers with
// at-least-once retry on NACK.
func (g *group) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-g.ch:
			g.dispatch(ctx, env)
		}
	}
}

// dispatch delivers env to every handler registered for its topic, retrying the WHOLE
// envelope on any handler error (at-least-once + ordering: a later message is not pulled
// until this one is acked by all handlers). It bails out on ctx cancellation.
func (g *group) dispatch(ctx context.Context, env envelope) {
	for {
		if ctx.Err() != nil {
			return
		}
		var failed bool
		for _, fn := range g.handlersFor(env.topic) {
			if err := fn(ctx, env.msg); err != nil {
				failed = true
				break // NACK: stop, redeliver the whole envelope (head-of-line, in order)
			}
		}
		if !failed {
			return // all handlers acked
		}
		// Redeliver after yielding, but stop if cancelled so a permanently NACKing
		// handler does not spin forever past shutdown.
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// Close marks the bus closed so further Publish returns [events.ErrBusClosed]. It does
// not stop in-flight Consume loops — cancel the Consume ctx for that. It is a
// convenience for tests/shutdown ordering.
func (b *Bus) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
}

// compile-time check: *Bus satisfies the full events.Bus seam.
var _ events.Bus = (*Bus)(nil)
