package iamv1_test

// redpandatest_test.go — a testcontainers-backed Redpanda harness for the Phase-2
// Kafka production path of the SDK event-bus stack. Redpanda is Kafka-API compatible
// and a single, lightweight binary (no ZooKeeper/KRaft cluster to boot), so it proves
// the kafkabus adapter end-to-end on a REAL broker without the weight of full Kafka.
//
// Docker-optional contract (mirrors pgtest_test.go): startRedpanda calls
// t.Skip("docker unavailable: ...") cleanly when testcontainers / Docker cannot start,
// so `go test ./...` on a machine WITHOUT Docker still passes (the Kafka tests skip,
// they do not fail). When Docker IS available (CI's ubuntu-latest, or a local Rancher/
// Docker), the TestKafka_ tests RUN against a real broker.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	tcredpanda "github.com/testcontainers/testcontainers-go/modules/redpanda"
)

var (
	rpOnce      sync.Once
	rpContainer *tcredpanda.Container
	rpSeed      string
	rpStartErr  error
)

// startRedpanda starts (once) a Redpanda broker via testcontainers and returns its
// Kafka seed-broker address. If Docker/testcontainers cannot start it SKIPS the calling
// test rather than failing, so the suite is green without Docker. The shared container
// is terminated in TestMain (pgtest_test.go) alongside the PG/MySQL containers, so it is
// reaped even with Ryuk disabled (the documented Rancher-Desktop local workflow).
func startRedpanda(t *testing.T) string {
	t.Helper()
	rpOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		c, err := tcredpanda.Run(ctx,
			"docker.redpanda.com/redpandadata/redpanda:v24.2.7",
			// Let the producer create the per-event-type topics on first publish (the
			// kafkabus producer sets AllowAutoTopicCreation); tests that need a specific
			// partition count create the topic explicitly via createTopic first.
			tcredpanda.WithAutoCreateTopics(),
		)
		if err != nil {
			rpStartErr = err
			return
		}
		seed, err := c.KafkaSeedBroker(ctx)
		if err != nil {
			rpStartErr = err
			_ = c.Terminate(ctx)
			return
		}
		rpContainer = c
		rpSeed = seed
	})
	if rpStartErr != nil {
		t.Skipf("docker unavailable: %v", rpStartErr)
	}
	return rpSeed
}

// terminateRedpanda reaps the shared Redpanda container; called from TestMain
// (pgtest_test.go) so cleanup is independent of Ryuk. No-op when never started.
func terminateRedpanda() {
	if rpContainer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = rpContainer.Terminate(ctx)
		cancel()
	}
}

// createTopic creates topic with the given partition count and replication factor 1
// (single-broker Redpanda) via the Kafka CreateTopics API, issued through a throwaway
// franz-go client. The two-consumer partition-split test needs a topic with >= 2
// partitions BEFORE any consumer joins, which auto-creation (1 partition) cannot give.
func createTopic(t *testing.T, seed, topic string, partitions int32) {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(seed))
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	defer cl.Close()

	req := kmsg.NewPtrCreateTopicsRequest()
	rt := kmsg.NewCreateTopicsRequestTopic()
	rt.Topic = topic
	rt.NumPartitions = partitions
	rt.ReplicationFactor = 1
	req.Topics = append(req.Topics, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := req.RequestWith(ctx, cl)
	if err != nil {
		t.Fatalf("create topic %q: %v", topic, err)
	}
	for _, tr := range resp.Topics {
		// code 36 == TOPIC_ALREADY_EXISTS, which is fine for an idempotent setup.
		if tr.ErrorCode != 0 && tr.ErrorCode != 36 {
			t.Fatalf("create topic %q: error code %d (%v)", topic, tr.ErrorCode, tr.ErrorMessage)
		}
	}
}

// partitionCountFor returns the number of partitions Kafka reports for topic, so a test
// can assert the broker actually gave the topic the partitions it created (and thus the
// two-consumer split is over real partitions).
func partitionCountFor(t *testing.T, seed, topic string) int {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(seed))
	if err != nil {
		t.Fatalf("metadata client: %v", err)
	}
	defer cl.Close()
	req := kmsg.NewPtrMetadataRequest()
	mt := kmsg.NewMetadataRequestTopic()
	mt.Topic = kmsg.StringPtr(topic)
	req.Topics = append(req.Topics, mt)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := req.RequestWith(ctx, cl)
	if err != nil {
		t.Fatalf("metadata for %q: %v", topic, err)
	}
	for _, tr := range resp.Topics {
		if tr.Topic != nil && *tr.Topic == topic {
			return len(tr.Partitions)
		}
	}
	return 0
}

// uniqueTopic derives a per-test topic name so parallel/sequential tests on the shared
// broker do not collide on partition layout or retained records.
func uniqueTopic(t *testing.T) string {
	return fmt.Sprintf("iam.test.%s.%d", sanitizeKafka(t.Name()), time.Now().UnixNano())
}

func sanitizeKafka(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
