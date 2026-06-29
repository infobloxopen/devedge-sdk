package rules_test

import (
	"context"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/health"
	"github.com/infobloxopen/devedge-sdk/rules"
)

// compile-time: a Cache is a health.Check.
var _ health.Check = (*rules.Cache[int])(nil)

// waitFor polls cond up to 2s; fails the test if it never holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCache_SnapshotThenWatch(t *testing.T) {
	src := rules.NewStaticSource[int]()
	src.Set("t1", 1)

	c := rules.NewCache("test", src)
	if c.Ready() {
		t.Fatal("cache ready before Run")
	}
	if err := c.Check(context.Background()); err == nil {
		t.Fatal("Check should fail before Run")
	}

	ctx := t.Context()
	go func() { _ = c.Run(ctx) }()

	waitFor(t, "ready after initial snapshot", c.Ready)
	if v, ok := c.Get("t1"); !ok || v != 1 {
		t.Fatalf("initial: Get(t1)=%d,%v want 1,true", v, ok)
	}
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check after ready: %v", err)
	}

	// Live update propagates through Watch.
	src.Set("t2", 2)
	waitFor(t, "t2 propagated", func() bool { _, ok := c.Get("t2"); return ok })
	if v, _ := c.Get("t2"); v != 2 {
		t.Fatalf("t2=%d, want 2", v)
	}

	// Delete propagates.
	src.Delete("t1")
	waitFor(t, "t1 deleted", func() bool { _, ok := c.Get("t1"); return !ok })
}

func TestCache_LastKnownGoodAfterSourceGone(t *testing.T) {
	src := rules.NewStaticSource[string]()
	src.Set("t1", "good")

	c := rules.NewCache("test", src)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Run(ctx) }()
	waitFor(t, "ready", c.Ready)

	// Source "goes away": stop the cache's sync loop.
	cancel()
	time.Sleep(20 * time.Millisecond)

	// Last-known-good is still served locally.
	if v, ok := c.Get("t1"); !ok || v != "good" {
		t.Fatalf("after source gone: Get(t1)=%q,%v want good,true", v, ok)
	}
}

// streamSource is a Source that does NOT implement Snapshotter — a pure event
// stream. Used to verify the Cache becomes ready on the first event.
type streamSource struct{ ch chan rules.Event[int] }

func (s *streamSource) Get(context.Context, string) (int, error) { return 0, rules.ErrNotFound }
func (s *streamSource) Watch(context.Context) (<-chan rules.Event[int], error) {
	return s.ch, nil
}

func TestCache_NonSnapshotter_ReadyOnFirstEvent(t *testing.T) {
	src := &streamSource{ch: make(chan rules.Event[int], 4)}
	c := rules.NewCache("stream", src)
	ctx := t.Context()
	go func() { _ = c.Run(ctx) }()

	// No snapshot capability → not ready until an event arrives.
	time.Sleep(20 * time.Millisecond)
	if c.Ready() {
		t.Fatal("non-snapshotter cache ready before any event")
	}
	src.ch <- rules.Event[int]{Tenant: "t1", Value: 9, Revision: 1}
	waitFor(t, "ready after first event", c.Ready)
	if v, ok := c.Get("t1"); !ok || v != 9 {
		t.Fatalf("Get(t1)=%d,%v want 9,true", v, ok)
	}
}

func TestCache_SkipsStaleEvents(t *testing.T) {
	src := &streamSource{ch: make(chan rules.Event[int], 4)}
	c := rules.NewCache("stream", src)
	ctx := t.Context()
	go func() { _ = c.Run(ctx) }()

	src.ch <- rules.Event[int]{Tenant: "t1", Value: 10, Revision: 5}
	waitFor(t, "rev5 applied", func() bool { v, ok := c.Get("t1"); return ok && v == 10 })

	// A stale event (revision <= highest applied) is ignored.
	src.ch <- rules.Event[int]{Tenant: "t1", Value: 99, Revision: 3}
	// A fresh event after it is applied — gives us a sync point.
	src.ch <- rules.Event[int]{Tenant: "t2", Value: 1, Revision: 6}
	waitFor(t, "rev6 applied", func() bool { _, ok := c.Get("t2"); return ok })

	if v, _ := c.Get("t1"); v != 10 {
		t.Fatalf("t1=%d, want 10 (stale rev3 should be ignored)", v)
	}
}
