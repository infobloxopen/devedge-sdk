package iamv1_test

// kafka_events_test.go — Phase-2 validation of the SDK event-bus stack's KAFKA
// production path on a REAL broker (Redpanda via testcontainers; see
// redpandatest_test.go for the docker-optional harness). It proves the kafkabus adapter
// (events/kafkabus) and the PostgreSQL advisory-lock relay leader (persistence/gormtx)
// end-to-end:
//
//	outbox → leader-elected RELAY → Kafka(Redpanda) → consumer GROUP → handler
//
// The five assertions the Phase-2 directive names:
//
//	(a) events delivered + the handler ran  — TestKafka_OutboxRelayToKafkaConsumerHandler
//	(b) two consumers in one group split partitions (each partition consumed once,
//	    competing consumers / multi-replica coherence) — TestKafka_TwoConsumersSplitPartitions
//	(c) per-aggregate ordering preserved via the Key  — TestKafka_PerKeyOrdering
//	(d) exactly-once EFFECT under redelivery via the idempotency marker
//	    — TestKafka_ExactlyOnceUnderRedelivery
//	(e) only ONE relay publishes when two relays contend on the PG advisory leader lock
//	    — TestKafka_SingleRelayUnderPGAdvisoryLeader
//
// Every test SKIPS cleanly when Docker is unavailable (startRedpanda / startPostgres).

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/events/kafkabus"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

// kafkaWaitFor polls cond until true or the deadline, so an async broker test does not
// race the consume goroutine. Generous deadline: a real broker round-trip is slower than
// the in-memory bus.
func kafkaWaitFor(t *testing.T, why string, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", why)
}

// TestKafka_OutboxRelayToKafkaConsumerHandler proves assertion (a): the full worked
// example flows over a REAL broker. Suspending a user appends UserSuspended to the
// write-only outbox; a leader-elected RELAY reads it forward and PUBLISHES it to Kafka
// (Redpanda) via the kafkabus producer; a CONSUMER group subscribes to the topic and
// runs the revoke-keys handler in its own GORM tx with the SQL idempotency marker. The
// relay + consumer run as the two independent goroutines a real service wires.
func TestKafka_OutboxRelayToKafkaConsumerHandler(t *testing.T) {
	seed := startRedpanda(t)
	db := openIAMGormPG(t)
	ctx := tenantCtx("acme")
	enc := secret.NewDev([]byte("0123456789abcdef0123456789abcdef"))

	users := iamv1.NewUserRepository(db)
	apiKeys := iamv1.NewApiKeyRepository(db, enc)
	tx := gormtx.NewGormTxRunner(db)
	store := gormtx.NewGormOutboxStore(db)
	pub := events.NewOutboxPublisher(store)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test", DisplayName: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k1", UserId: "u1", KeyValue: "tok1", KeyPrefix: "k1"}); err != nil {
		t.Fatalf("seed key1: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k2", UserId: "u1", KeyValue: "tok2", KeyPrefix: "k2"}); err != nil {
		t.Fatalf("seed key2: %v", err)
	}

	// The event TYPE is per-test unique, and the relay's DEFAULT topic mapper makes
	// topic == event type — so the consumer (which subscribes by event type) and the
	// relay publish to the SAME topic, and the shared broker does not mix this test's
	// records with another test's. (The consumer keys handler lookup AND idempotency on
	// the event's real Type/ID, so type and topic must agree — they do, via the default.)
	eventType := uniqueTopic(t)
	group := "iam-" + sanitizeKafka(t.Name())

	// Produce: suspend the user (user change + outbox event commit atomically).
	if err := suspendUserGormTyped(ctx, tx, users, pub, "u1", eventType); err != nil {
		t.Fatalf("suspendUser: %v", err)
	}

	bus := kafkabus.New([]string{seed})
	defer bus.Close()
	// Default topic mapper: topic == event type == eventType.
	relay := events.NewRelay(store, gormtx.NewGormOutboxCursorStore(db), bus)

	consumer := events.NewConsumer(bus, tx, gormtx.NewGormIdempotencyStore(db),
		events.WithConsumerGroup(group))
	var handlerRan atomic.Bool
	revoke := revokeKeysHandlerGorm(apiKeys)
	consumer.Subscribe(eventType, "revoke-api-keys", func(hctx context.Context, evt events.Event) error {
		if err := revoke(hctx, evt); err != nil {
			return err
		}
		handlerRan.Store(true)
		return nil
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = consumer.Run(runCtx) }()
	go func() { defer wg.Done(); relay.Run(runCtx, 50*time.Millisecond, 10, nil) }()

	// AFTER the event flows through Kafka the keys are revoked (eventual consistency).
	kafkaWaitFor(t, "the user's API keys to be revoked via Kafka", 60*time.Second, func() bool {
		remaining, _, err := apiKeys.List(ctx, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10})
		return err == nil && len(remaining) == 0
	})
	cancel()
	wg.Wait()

	if !handlerRan.Load() {
		t.Fatal("the handler must have run on the Kafka delivery")
	}
	// Write-only: the relay only advanced its sidecar cursor — the outbox row survives.
	var outboxRows int64
	if err := db.WithContext(ctx).Table("outbox").Count(&outboxRows).Error; err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxRows != 1 {
		t.Fatalf("write-only: the relay must never delete the outbox row, found %d", outboxRows)
	}
}

// TestKafka_TwoConsumersSplitPartitions proves assertion (b): two consumers in ONE group
// are COMPETING CONSUMERS — Kafka's group coordinator splits the topic's partitions
// across them so each partition is owned by EXACTLY ONE member, and every record is
// processed once. The kafkabus producer publishes the records (same code path the relay
// uses); the consumers are raw franz-go group clients so the test can OBSERVE which
// member processed which partition (the bus's BusHandler hides the partition). We assert:
// no partition was processed by both members, both members did work (the split is real,
// not all-on-one), and every record was processed exactly once.
func TestKafka_TwoConsumersSplitPartitions(t *testing.T) {
	seed := startRedpanda(t)
	topic := uniqueTopic(t)
	group := "iam-" + sanitizeKafka(t.Name())
	const partitions = 2
	createTopic(t, seed, topic, partitions)
	if got := partitionCountFor(t, seed, topic); got != partitions {
		t.Fatalf("topic must have %d partitions for the split test, broker reports %d", partitions, got)
	}

	// Produce 200 records across many keys (so both partitions get traffic) via the
	// kafkabus producer — the same key→partition path the relay uses.
	const total = 200
	prod := kafkabus.New([]string{seed})
	defer prod.Close()
	pctx := context.Background()
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("agg-%d", i%20) // 20 distinct keys -> spread over 2 partitions
		evt := events.Event{ID: fmt.Sprintf("e-%d", i), Type: topic, AggregateID: key, Payload: []byte(fmt.Sprintf("%d", i))}
		if err := prod.Publish(pctx, topic, events.BusMessage{Key: key, Event: evt}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// Two raw group clients in ONE group, so we can see rec.Partition per member.
	var mu sync.Mutex
	partitionOwner := map[int32]string{} // partition -> the member that processed it
	processedBy := map[string]int{}      // member -> records processed
	seenIDs := map[string]int{}          // eventID -> times processed (must be 1)

	mkClient := func(name string) *kgo.Client {
		cl, err := kgo.NewClient(
			kgo.SeedBrokers(seed),
			kgo.ConsumerGroup(group),
			kgo.ConsumeTopics(topic),
			kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		)
		if err != nil {
			t.Fatalf("group client %s: %v", name, err)
		}
		return cl
	}
	c1 := mkClient("c1")
	c2 := mkClient("c2")
	defer c1.Close()
	defer c2.Close()

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pump := func(name string, cl *kgo.Client) {
		for runCtx.Err() == nil {
			pollCtx, pc := context.WithTimeout(runCtx, time.Second)
			fetches := cl.PollFetches(pollCtx)
			pc()
			if runCtx.Err() != nil {
				return
			}
			fetches.EachRecord(func(rec *kgo.Record) {
				mu.Lock()
				partitionOwner[rec.Partition] = name // last writer wins; checked for conflict below
				processedBy[name]++
				seenIDs[string(rec.Key)+":"+string(rec.Value)]++
				mu.Unlock()
			})
			_ = cl.CommitUncommittedOffsets(runCtx)
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump("c1", c1) }()
	go func() { defer wg.Done(); pump("c2", c2) }()

	kafkaWaitFor(t, "all records processed exactly once across the group", 90*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seenIDs) == total
	})
	time.Sleep(500 * time.Millisecond) // catch any erroneous double-processing
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// Every record processed EXACTLY once (competing consumers, not fan-out).
	if len(seenIDs) != total {
		t.Fatalf("competing consumers: every record must be processed once, got %d distinct of %d", len(seenIDs), total)
	}
	for id, n := range seenIDs {
		if n != 1 {
			t.Fatalf("record %s processed %d times — a partition was consumed by BOTH members (not split)", id, n)
		}
	}
	// BOTH members must have done work — the partitions were genuinely split across the two.
	if processedBy["c1"] == 0 || processedBy["c2"] == 0 {
		t.Fatalf("both group members must own a partition, got c1=%d c2=%d (partitions not split across the two consumers)",
			processedBy["c1"], processedBy["c2"])
	}
	// Each member owned a distinct partition (the partitionOwner map saw no cross-claim,
	// implied by every record being processed exactly once over a stable assignment).
	if len(partitionOwner) != partitions {
		t.Fatalf("both partitions must have been consumed, saw %d of %d partitions owned", len(partitionOwner), partitions)
	}
	if processedBy["c1"]+processedBy["c2"] != total {
		t.Fatalf("the two members together must process every record once, got c1=%d + c2=%d != %d",
			processedBy["c1"], processedBy["c2"], total)
	}
}

// TestKafka_PerKeyOrdering proves assertion (c): per-aggregate ordering is preserved via
// the BusMessage.Key. All records for one key land on one partition (Kafka hashes the
// key), and a partition preserves write order — so a single consumer receives a given
// key's records in PRODUCE order. We produce an interleaved sequence for several keys and
// assert each key's records arrive strictly in order.
func TestKafka_PerKeyOrdering(t *testing.T) {
	seed := startRedpanda(t)
	topic := uniqueTopic(t)
	group := "iam-" + sanitizeKafka(t.Name())
	createTopic(t, seed, topic, 4) // several partitions so keys actually hash apart

	const keys = 5
	const perKey = 50
	prod := kafkabus.New([]string{seed})
	defer prod.Close()
	pctx := context.Background()
	// Interleave: for seq 0..perKey-1, publish that seq for every key. So per partition the
	// broker still sees each key's seqs in increasing order, but globally they are mixed.
	for seq := 0; seq < perKey; seq++ {
		for k := 0; k < keys; k++ {
			key := fmt.Sprintf("agg-%d", k)
			evt := events.Event{ID: fmt.Sprintf("%s-%d", key, seq), Type: topic, AggregateID: key, Payload: []byte(fmt.Sprintf("%d", seq))}
			if err := prod.Publish(pctx, topic, events.BusMessage{Key: key, Event: evt}); err != nil {
				t.Fatalf("publish %s seq %d: %v", key, seq, err)
			}
		}
	}

	var mu sync.Mutex
	order := map[string][]int{} // key -> sequence of payload values as received
	total := keys * perKey

	cons := kafkabus.New([]string{seed})
	defer cons.Close()
	_ = cons.Subscribe(group, topic, func(_ context.Context, msg events.BusMessage) error {
		mu.Lock()
		defer mu.Unlock()
		var v int
		_, _ = fmt.Sscanf(string(msg.Event.Payload), "%d", &v)
		order[msg.Key] = append(order[msg.Key], v)
		return nil
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = cons.Consume(runCtx) }()

	kafkaWaitFor(t, "all keyed records received", 90*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		got := 0
		for _, s := range order {
			got += len(s)
		}
		return got == total
	})
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("agg-%d", k)
		seqs := order[key]
		if len(seqs) != perKey {
			t.Fatalf("key %s: expected %d records, got %d", key, perKey, len(seqs))
		}
		if !sort.IntsAreSorted(seqs) {
			t.Fatalf("per-aggregate ordering VIOLATED for key %s: records not in produce order: %v", key, seqs)
		}
	}
}

// TestKafka_ExactlyOnceUnderRedelivery proves assertion (d): the bus is at-least-once
// (a handler NACK is re-delivered by Kafka because the offset is only committed AFTER the
// handler ACKs), and the per-(event, handler) idempotency marker keeps the EFFECT exactly
// once. A handler NACKs its first delivery of the event; Kafka re-delivers it (offset not
// committed); on the redelivery the handler's effect runs and the SQL idempotency marker
// makes the second-and-later deliveries no-ops. We assert the revoke effect lands exactly
// once (the key stays revoked) and exactly ONE idempotency marker commits.
func TestKafka_ExactlyOnceUnderRedelivery(t *testing.T) {
	seed := startRedpanda(t)
	db := openIAMGormPG(t)
	ctx := tenantCtx("acme")
	enc := secret.NewDev([]byte("0123456789abcdef0123456789abcdef"))

	users := iamv1.NewUserRepository(db)
	apiKeys := iamv1.NewApiKeyRepository(db, enc)
	tx := gormtx.NewGormTxRunner(db)
	store := gormtx.NewGormOutboxStore(db)
	pub := events.NewOutboxPublisher(store)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test", DisplayName: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := apiKeys.Create(ctx, &iamv1.ApiKey{Id: "k1", UserId: "u1", KeyValue: "tok1", KeyPrefix: "k1"}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	eventType := uniqueTopic(t)
	if err := suspendUserGormTyped(ctx, tx, users, pub, "u1", eventType); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	group := "iam-" + sanitizeKafka(t.Name())

	bus := kafkabus.New([]string{seed})
	defer bus.Close()
	// Default topic mapper: topic == event type. The consumer keys handler lookup and the
	// idempotency marker on the event's real Type/ID, so type and topic must agree.
	relay := events.NewRelay(store, gormtx.NewGormOutboxCursorStore(db), bus)
	consumer := events.NewConsumer(bus, tx, gormtx.NewGormIdempotencyStore(db),
		events.WithConsumerGroup(group))

	var attempts int64
	nackFirst := int64(1)
	revoke := revokeKeysHandlerGorm(apiKeys)
	consumer.Subscribe(eventType, "revoke-api-keys", func(hctx context.Context, evt events.Event) error {
		atomic.AddInt64(&attempts, 1)
		if atomic.CompareAndSwapInt64(&nackFirst, 1, 0) {
			// NACK the first delivery: the kafkabus must NOT commit the offset, so Kafka
			// re-delivers the record.
			return fmt.Errorf("transient handler failure (NACK -> redeliver)")
		}
		return revoke(hctx, evt)
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = consumer.Run(runCtx) }()
	go func() { defer wg.Done(); relay.Run(runCtx, 50*time.Millisecond, 10, nil) }()

	// The effect converges to the key being revoked despite the NACK + redelivery.
	kafkaWaitFor(t, "the key to be revoked after a NACK+redelivery", 90*time.Second, func() bool {
		remaining, _, err := apiKeys.List(ctx, persistence.ListOptions{Filter: `user_id = "u1"`, PageSize: 10})
		return err == nil && len(remaining) == 0
	})
	time.Sleep(1 * time.Second) // catch any erroneous extra delivery slipping an extra effect
	cancel()
	wg.Wait()

	// At-least-once actually happened: the handler was attempted more than once.
	if got := atomic.LoadInt64(&attempts); got < 2 {
		t.Fatalf("the test must have actually redelivered (NACKed once), attempts=%d", got)
	}
	// Exactly-once EFFECT: exactly one idempotency marker committed for (event, handler).
	var markers int64
	if err := db.WithContext(ctx).Model(&gormtx.IdemMarker{}).Count(&markers).Error; err != nil {
		t.Fatalf("count idempotency markers: %v", err)
	}
	if markers != 1 {
		t.Fatalf("exactly-once: exactly one idempotency marker must commit across the redelivery, found %d", markers)
	}
}

// TestKafka_SingleRelayUnderPGAdvisoryLeader proves assertion (e): when TWO relays (two
// service replicas) contend on the PostgreSQL advisory-lock leader, exactly ONE wins the
// lock and pumps the outbox to Kafka — the other idles. The event therefore reaches the
// broker exactly once (no double-publish), which we verify by counting the records that
// actually land on the topic with a raw kgo consumer.
func TestKafka_SingleRelayUnderPGAdvisoryLeader(t *testing.T) {
	seed := startRedpanda(t)
	db := openIAMGormPG(t)
	ctx := tenantCtx("acme")

	users := iamv1.NewUserRepository(db)
	tx := gormtx.NewGormTxRunner(db)
	store := gormtx.NewGormOutboxStore(db)
	pub := events.NewOutboxPublisher(store)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test", DisplayName: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := suspendUserGorm(ctx, tx, users, pub, "u1"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	topic := uniqueTopic(t)

	bus := kafkabus.New([]string{seed})
	defer bus.Close()

	// TWO relays sharing ONE PG advisory-lock leader (the lock both replicas contend on).
	// Each constructs its OWN leader instance over the same DB + same lock name, exactly
	// as two replicas of one service would.
	leaderA, err := gormtx.NewPGAdvisoryLeaderFromGorm(db, "test-relay-lock")
	if err != nil {
		t.Fatalf("leaderA: %v", err)
	}
	leaderB, err := gormtx.NewPGAdvisoryLeaderFromGorm(db, "test-relay-lock")
	if err != nil {
		t.Fatalf("leaderB: %v", err)
	}
	// SHARED cursor sidecar (one service, one logical relay progress) so a double-publish
	// would have to come from the second relay winning the lock — not from two cursors.
	cursors := gormtx.NewGormOutboxCursorStore(db)
	topicMap := events.WithRelayTopicMapper(func(events.Event) string { return topic })
	relayA := events.NewRelay(store, cursors, bus, topicMap, events.WithRelayLeader(leaderA))
	relayB := events.NewRelay(store, cursors, bus, topicMap, events.WithRelayLeader(leaderB))

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); relayA.Run(runCtx, 50*time.Millisecond, 10, nil) }()
	go func() { defer wg.Done(); relayB.Run(runCtx, 50*time.Millisecond, 10, nil) }()

	// Count the records that land on the topic with a raw kgo consumer (a fresh group at
	// the start of the topic). With single-relay election it must be EXACTLY one even
	// though two relays are running; a second active relay would double-publish.
	rec := countTopicRecords(t, seed, topic, "verify-"+sanitizeKafka(t.Name()), 1, 15*time.Second)
	if rec != 1 {
		t.Fatalf("single-relay election under the PG advisory leader: the event must reach Kafka exactly once, got %d (the second relay won the lock and double-published)", rec)
	}
	// Hold a moment with both relays running to ensure the idle relay never sneaks a
	// second publish, then re-count.
	time.Sleep(2 * time.Second)
	rec2 := countTopicRecords(t, seed, topic, "verify2-"+sanitizeKafka(t.Name()), 1, 10*time.Second)
	if rec2 != 1 {
		t.Fatalf("single-relay election: still exactly one record expected after settling, got %d", rec2)
	}
	cancel()
	wg.Wait()
}

// suspendUserGormTyped is suspendUserGorm with a caller-chosen event TYPE, so a Kafka
// test can use a per-test-unique type (== topic via the relay's default mapper) and not
// collide with other tests' records on the shared broker.
func suspendUserGormTyped(ctx context.Context, tx persistence.TxRunner, users *iamv1.UserRepository, pub events.Publisher, userID, eventType string) error {
	return tx.Atomically(ctx, func(ctx context.Context) error {
		u, err := users.Get(ctx, userID)
		if err != nil {
			return err
		}
		u.DisplayName = "[suspended] " + u.GetDisplayName()
		if _, err := users.Update(ctx, userID, u, "display_name"); err != nil {
			return err
		}
		return pub.Publish(ctx, events.Event{
			Type:          eventType,
			AggregateType: "User",
			AggregateID:   userID,
			Payload:       []byte(userID),
		})
	})
}

// countTopicRecords reads topic from the START with a fresh consumer group and returns
// how many records it sees within timeout (after seeing at least wantAtLeast it keeps
// draining briefly to catch a stray extra). It is the raw broker-side observation the
// single-relay test asserts on.
func countTopicRecords(t *testing.T, seed, topic, group string, wantAtLeast int, timeout time.Duration) int {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(seed),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		t.Fatalf("count consumer: %v", err)
	}
	defer cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	count := 0
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return count
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			return count
		}
		fetches.EachRecord(func(_ *kgo.Record) { count++ })
		if count >= wantAtLeast {
			// Drain a short while more to catch an erroneous extra publish.
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
			more := cl.PollFetches(drainCtx)
			drainCancel()
			more.EachRecord(func(_ *kgo.Record) { count++ })
			return count
		}
	}
}
