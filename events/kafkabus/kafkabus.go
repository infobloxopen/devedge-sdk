// Package kafkabus is the Kafka adapter for the [events.Bus] seam — Phase 2 of the
// SDK event-bus stack. It carries events from the relay (which reads the write-only
// outbox) to the consumer (which propagates them to the registered handlers) over a
// REAL broker, so the same outbox→relay→bus→consumer→handler pipeline that runs
// in-process on [github.com/infobloxopen/devedge-sdk/events/membus] now spans
// replicas: a relay on one pod publishes to Kafka and a consumer on another pod (a
// member of the consumer group) receives it.
//
// CLEAN CORE: this adapter is the ONLY package in the SDK that imports a broker
// dependency (franz-go, github.com/twmb/franz-go). The [events] core, [persistence],
// [authz], and [grpcauthz] stay pure Go — they speak the backend-neutral [events.Bus]
// interface and never see Kafka. (Verified in CI: `go list -deps ./events ./persistence
// ./authz/... ./grpcauthz/...` shows no franz/kafka.)
//
// franz-go is pure Go (no CGo), so it fits the SDK's CGo-free test/build posture.
//
// DESIGN — Publish (producer):
//
//	A single shared *kgo.Client produces each [events.BusMessage] as one Kafka record
//	whose Record.Key == BusMessage.Key. Kafka's default hash partitioner sends all
//	records with the same key to the SAME partition, and a partition preserves write
//	order — so every event of one aggregate (the relay keys by AggregateID) lands on
//	one partition and keeps its commit order. Publish is SYNCHRONOUS and waits for the
//	broker ack (RequiredAcks=AllISR) before returning, so the relay only advances its
//	outbox cursor once the broker has durably accepted the event (matching the
//	at-least-once contract: a failed Publish leaves the cursor un-advanced and the relay
//	re-publishes).
//
// DESIGN — Subscribe/Consume (consumer group):
//
//	Subscribe records (group, topic) -> handler and is a setup-time call. Consume opens
//	ONE *kgo.Client per consumer group, joined to that Kafka consumer group and
//	subscribed to every topic Subscribe registered for the group. Multiple service
//	replicas that Consume the same group are COMPETING CONSUMERS: Kafka's group
//	coordinator splits the topic partitions across them (each partition is owned by
//	exactly one member), so a message is processed by exactly one replica — the
//	multi-replica coherence the membus could not give.
//
//	OFFSET COMMIT IS AFTER THE HANDLER (at-least-once, never lose): auto-commit is
//	DISABLED. The consume loop polls a batch, runs each record's handler (which, in the
//	[events.Consumer], commits the handler's aggregate write + the idempotency marker in
//	one tx), and only AFTER the handler ACKs does it commit that record's offset. If the
//	process crashes between the handler commit and the offset commit, the offset is still
//	behind the record, so on restart (or rebalance to another member) the record is
//	re-delivered — and the idempotency marker makes the re-delivery a no-op EFFECT
//	(exactly-once effect over an at-least-once transport). A handler that NACKs (returns
//	an error) is NOT committed: the loop stops advancing that partition and the record is
//	re-polled/re-delivered, so a transient failure is retried rather than skipped.
package kafkabus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/infobloxopen/devedge-sdk/events"
)

// Bus is the Kafka-backed [events.Bus]: a shared producer client plus, per consumer
// group, a consumer-group client that polls and dispatches with offset-commit-after-handler.
type Bus struct {
	seeds       []string
	clientID    string
	produceAcks kgo.Acks
	pollTimeout time.Duration

	producerOnce sync.Once
	producer     *kgo.Client
	producerErr  error

	mu       sync.Mutex
	subs     map[string]map[string]events.BusHandler // group -> topic -> handler
	closed   bool
	clients  []*kgo.Client // every client we open, closed on Close
}

// Option configures a [Bus].
type Option func(*Bus)

// WithClientID sets the Kafka client id reported to the broker (for observability).
// Defaults to "devedge-sdk".
func WithClientID(id string) Option {
	return func(b *Bus) {
		if id != "" {
			b.clientID = id
		}
	}
}

// WithRequiredAcks sets the producer ack level. The default is [kgo.AllISRAcks]
// (wait for all in-sync replicas) so a Publish that returns nil is durably committed
// — the strongest at-least-once guarantee. Relax it only if a service accepts weaker
// durability for throughput.
func WithRequiredAcks(acks kgo.Acks) Option {
	return func(b *Bus) { b.produceAcks = acks }
}

// WithPollTimeout bounds how long a single Consume poll blocks before looping (so the
// loop checks ctx cancellation promptly). Defaults to 1s.
func WithPollTimeout(d time.Duration) Option {
	return func(b *Bus) {
		if d > 0 {
			b.pollTimeout = d
		}
	}
}

// New returns a Kafka bus that produces to / consumes from the brokers at seeds (e.g.
// "localhost:9092"). It does not connect until the first Publish or Consume.
func New(seeds []string, opts ...Option) *Bus {
	b := &Bus{
		seeds:       append([]string(nil), seeds...),
		clientID:    "devedge-sdk",
		produceAcks: kgo.AllISRAcks(),
		pollTimeout: time.Second,
		subs:        make(map[string]map[string]events.BusHandler),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// getProducer lazily opens the shared producer client. RequiredAcks=AllISR makes a
// returned-nil Publish durable; AllowAutoTopicCreation lets a fresh topic be created on
// first publish (so a dev/test broker need not pre-create the per-event-type topics).
func (b *Bus) getProducer() (*kgo.Client, error) {
	b.producerOnce.Do(func() {
		cl, err := kgo.NewClient(
			kgo.SeedBrokers(b.seeds...),
			kgo.ClientID(b.clientID),
			kgo.RequiredAcks(b.produceAcks),
			kgo.AllowAutoTopicCreation(),
			// AllISR acks require idempotent-producer disabled-or-acks-all consistency;
			// franz-go enables the idempotent producer by default with AllISR, which is
			// exactly the per-partition ordering + de-dup we want for the relay.
		)
		if err != nil {
			b.producerErr = err
			return
		}
		b.mu.Lock()
		b.producer = cl
		b.clients = append(b.clients, cl)
		b.mu.Unlock()
	})
	return b.producer, b.producerErr
}

// Publish implements [events.BusPublisher]: produce msg to topic as one Kafka record
// keyed by msg.Key (so per-key/per-aggregate order is preserved on a single partition).
// It blocks until the broker acks the record (RequiredAcks), so a nil return means the
// event is durably on the broker — the relay then advances its outbox cursor. A produce
// error is returned so the relay leaves the cursor un-advanced and re-publishes.
func (b *Bus) Publish(ctx context.Context, topic string, msg events.BusMessage) error {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return events.ErrBusClosed
	}
	producer, err := b.getProducer()
	if err != nil {
		return fmt.Errorf("kafkabus: open producer: %w", err)
	}
	rec := &kgo.Record{
		Topic: topic,
		Key:   []byte(msg.Key),
		Value: msg.Event.Payload,
		Headers: []kgo.RecordHeader{
			{Key: hdrEventID, Value: []byte(msg.Event.ID)},
			{Key: hdrEventType, Value: []byte(msg.Event.Type)},
			{Key: hdrAggregateType, Value: []byte(msg.Event.AggregateType)},
			{Key: hdrAggregateID, Value: []byte(msg.Event.AggregateID)},
			{Key: hdrAccountID, Value: []byte(msg.Event.AccountID)},
		},
	}
	// Synchronous produce: wait for the broker ack so the relay's cursor only advances
	// after the event is durable.
	res := producer.ProduceSync(ctx, rec)
	if perr := res.FirstErr(); perr != nil {
		return fmt.Errorf("kafkabus: produce to %q: %w", topic, perr)
	}
	return nil
}

// Subscribe implements [events.BusSubscriber]: record fn as the handler for (group,
// topic). It is a setup-time call made before Consume — the consumer group client is not
// opened until Consume, so all Subscribe calls must precede it. Re-subscribing the same
// (group, topic) replaces the handler (the [events.Consumer] subscribes one bus handler
// per topic that fans to its domain handlers, so one handler per topic is the contract).
func (b *Bus) Subscribe(group, topic string, fn events.BusHandler) error {
	if fn == nil {
		panic("kafkabus: Subscribe called with a nil handler for topic " + topic)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[group] == nil {
		b.subs[group] = make(map[string]events.BusHandler)
	}
	b.subs[group][topic] = fn
	return nil
}

// Consume implements [events.BusSubscriber]: open one consumer-group client per group
// registered via Subscribe, then block delivering records to the registered handlers
// until ctx is cancelled. Each group client joins the Kafka consumer group (competing
// consumers across replicas) and subscribes to every topic the group registered.
//
// Returns ctx.Err() on cancellation (nil if ctx had no deadline/cancel and a group loop
// returned), or the first group-loop error.
func (b *Bus) Consume(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return events.ErrBusClosed
	}
	// Snapshot the group->topic->handler registrations.
	groups := make(map[string]map[string]events.BusHandler, len(b.subs))
	for g, topics := range b.subs {
		cp := make(map[string]events.BusHandler, len(topics))
		for t, fn := range topics {
			cp[t] = fn
		}
		groups[g] = cp
	}
	b.mu.Unlock()

	if len(groups) == 0 {
		// Nothing subscribed: behave like the membus (block until cancelled).
		<-ctx.Done()
		return ctx.Err()
	}

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for group, topics := range groups {
		cl, err := b.openGroupClient(group, topics)
		if err != nil {
			return fmt.Errorf("kafkabus: open consumer group %q: %w", group, err)
		}
		wg.Add(1)
		go func(group string, topics map[string]events.BusHandler, cl *kgo.Client) {
			defer wg.Done()
			if gerr := b.consumeGroup(ctx, cl, topics); gerr != nil && !errors.Is(gerr, context.Canceled) {
				errOnce.Do(func() { firstErr = gerr })
			}
		}(group, topics, cl)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// openGroupClient opens (and tracks for Close) one consumer-group client. Auto-commit
// is DISABLED — the loop commits AFTER the handler ACKs (at-least-once, commit-after).
// BlockRebalanceOnPoll keeps a rebalance from yanking partitions mid-batch, so an
// in-flight record's offset is committed before the partition can move to another member.
func (b *Bus) openGroupClient(group string, topics map[string]events.BusHandler) (*kgo.Client, error) {
	topicNames := make([]string, 0, len(topics))
	for t := range topics {
		topicNames = append(topicNames, t)
	}
	sort.Strings(topicNames) // deterministic for tests/logs
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(b.seeds...),
		kgo.ClientID(b.clientID),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topicNames...),
		kgo.DisableAutoCommit(),    // commit AFTER the handler, manually
		kgo.BlockRebalanceOnPoll(), // hold partitions across a poll+commit cycle
		// Start a brand-new group at the earliest offset so a consumer that joins after
		// the relay produced still sees the backlog (matches the outbox at-least-once
		// expectation; a committed group resumes from its committed offset).
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.clients = append(b.clients, cl)
	b.mu.Unlock()
	return cl, nil
}

// consumeGroup is the per-group poll loop. It polls a bounded batch, dispatches each
// record to the topic's handler IN PARTITION ORDER, and commits a record's offset only
// after the handler ACKs. On the first NACK in a partition it stops that partition (the
// failing record and everything after it is NOT committed), and re-seeks the partition's
// consume position BACK to the failed record so the next poll RE-DELIVERS it in-session
// (at-least-once with bounded head-of-line, mirroring the membus and the relay's cursor).
// The per-(event, handler) idempotency marker in the consumer keeps the EFFECT
// exactly-once across the redelivery.
func (b *Bus) consumeGroup(ctx context.Context, cl *kgo.Client, topics map[string]events.BusHandler) error {
	defer cl.AllowRebalance() // ensure a final poll's rebalance block is released
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pollCtx, cancel := context.WithTimeout(ctx, b.pollTimeout)
		fetches := cl.PollFetches(pollCtx)
		cancel()
		if ctx.Err() != nil {
			cl.AllowRebalance()
			return ctx.Err()
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			// A poll-timeout shows up as context.DeadlineExceeded on the inner ctx — that
			// is the normal "no records this tick" path, not a failure. Surface anything else.
			for _, fe := range errs {
				if errors.Is(fe.Err, context.DeadlineExceeded) || errors.Is(fe.Err, context.Canceled) {
					continue
				}
				cl.AllowRebalance()
				return fmt.Errorf("kafkabus: poll fetches: %w", fe.Err)
			}
		}

		// Commit-after-handler, per partition, in order. EachPartition hands records in
		// offset order; we run handlers and collect the records to commit (those whose
		// handler ACKed up to the first NACK in that partition). A NACK records the
		// seek-back position for that partition so we re-deliver from the failed record.
		var toCommit []*kgo.Record
		// reseek: topic -> partition -> EpochOffset to rewind the consume position to (the
		// NACKed record), so the next poll re-fetches it (in-session at-least-once).
		reseek := map[string]map[int32]kgo.EpochOffset{}
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			for _, rec := range p.Records {
				handler := topics[rec.Topic]
				if handler == nil {
					// No handler for this topic in this group (should not happen — we only
					// subscribed registered topics) — treat as ACK so we do not wedge.
					toCommit = append(toCommit, rec)
					continue
				}
				if herr := handler(ctx, recordToBusMessage(rec)); herr != nil {
					// NACK: do NOT commit this record (or anything after it in this
					// partition). Rewind the partition's consume position to THIS record so
					// the next poll re-delivers it; stop processing this partition (bounded
					// head-of-line). The idempotency marker keeps the effect exactly-once.
					if reseek[rec.Topic] == nil {
						reseek[rec.Topic] = map[int32]kgo.EpochOffset{}
					}
					reseek[rec.Topic][rec.Partition] = kgo.EpochOffset{Epoch: rec.LeaderEpoch, Offset: rec.Offset}
					break
				}
				toCommit = append(toCommit, rec)
			}
		})

		// Release the rebalance block now that we have run the handlers for this batch, so
		// the group can rebalance before our (blocking) commit / next poll.
		cl.AllowRebalance()

		if len(toCommit) > 0 {
			// Commit AFTER the handlers ACKed — the offset only advances past durably
			// handled records. A commit failure leaves the offset behind for re-delivery.
			if cerr := cl.CommitRecords(ctx, toCommit...); cerr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("kafkabus: commit offsets: %w", cerr)
			}
		}
		// Re-seek any NACKed partitions back to the failed record so the next poll
		// re-delivers it in-session (the consume position, not the committed offset, drives
		// what PollFetches returns next). SetOffsets is called here — outside PollFetches,
		// after the commit, with the rebalance block released — exactly as franz-go advises.
		if len(reseek) > 0 {
			cl.SetOffsets(reseek)
		}
	}
}

// Close releases the producer and all consumer-group clients. It marks the bus closed so
// further Publish returns [events.ErrBusClosed]. Cancel the Consume ctx first so the
// consume loops have exited before Close.
func (b *Bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	clients := append([]*kgo.Client(nil), b.clients...)
	b.mu.Unlock()
	for _, cl := range clients {
		cl.Close()
	}
}

// recordToBusMessage reconstructs the [events.BusMessage] from a Kafka record: the key is
// the partition/ordering key and the headers carry the [events.Event] envelope fields
// (the payload is the record value).
func recordToBusMessage(rec *kgo.Record) events.BusMessage {
	evt := events.Event{Payload: rec.Value}
	for _, h := range rec.Headers {
		switch h.Key {
		case hdrEventID:
			evt.ID = string(h.Value)
		case hdrEventType:
			evt.Type = string(h.Value)
		case hdrAggregateType:
			evt.AggregateType = string(h.Value)
		case hdrAggregateID:
			evt.AggregateID = string(h.Value)
		case hdrAccountID:
			evt.AccountID = string(h.Value)
		}
	}
	return events.BusMessage{Key: string(rec.Key), Event: evt}
}

// Kafka record header keys for the event envelope fields. The payload travels as the
// record value; everything else the [events.Event] carries rides in headers so the
// consumer reconstructs the exact envelope (the ID is the idempotency key).
const (
	hdrEventID       = "devedge-event-id"
	hdrEventType     = "devedge-event-type"
	hdrAggregateType = "devedge-aggregate-type"
	hdrAggregateID   = "devedge-aggregate-id"
	hdrAccountID     = "devedge-account-id"
)

// compile-time check: *Bus satisfies the full events.Bus seam.
var _ events.Bus = (*Bus)(nil)
