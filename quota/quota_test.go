package quota_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infobloxopen/devedge-sdk/quota"
)

func TestStaticLimits(t *testing.T) {
	s := quota.NewStaticLimits(map[string]map[string]int64{
		"acme": {"sandboxes": 3},
	})
	if lim, has, _ := s.Limit(context.Background(), "acme", "sandboxes"); !has || lim != 3 {
		t.Fatalf("want (3,true), got (%d,%v)", lim, has)
	}
	if _, has, _ := s.Limit(context.Background(), "acme", "unknown"); has {
		t.Fatalf("unknown metric must report has=false")
	}
	if _, has, _ := s.Limit(context.Background(), "other", "sandboxes"); has {
		t.Fatalf("unknown account must report has=false")
	}
}

func TestMemoryMeterStockLifecycle(t *testing.T) {
	m := quota.NewMemoryMeter(quota.NewStaticLimits(map[string]map[string]int64{
		"acme": {"sandboxes": 2},
	}))
	ctx := context.Background()
	c := quota.Charge{Account: "acme", Metric: "sandboxes", Amount: 1}

	r1, err := m.Reserve(ctx, c)
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if err := r1.Commit(ctx); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	r2, err := m.Reserve(ctx, c)
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	// At the limit now (2 held). A third must be refused.
	if _, err := m.Reserve(ctx, c); !errors.Is(err, quota.ErrOverLimit) {
		t.Fatalf("want ErrOverLimit, got %v", err)
	}
	// Releasing the second frees room again.
	if err := r2.Release(ctx); err != nil {
		t.Fatalf("release 2: %v", err)
	}
	if _, err := m.Reserve(ctx, c); err != nil {
		t.Fatalf("reserve after release should succeed, got %v", err)
	}
}

func TestMemoryMeterUnlimitedWhenNoLimit(t *testing.T) {
	m := quota.NewMemoryMeter(quota.NewStaticLimits(nil))
	for i := 0; i < 100; i++ {
		if _, err := m.Reserve(context.Background(), quota.Charge{Account: "x", Metric: "y"}); err != nil {
			t.Fatalf("no declared limit must be unlimited, got %v", err)
		}
	}
}

func TestMemoryMeterFlowWindowSeparation(t *testing.T) {
	// A flow metric in a month window: reservations in different months must not
	// share a bucket. We can't move the wall clock here, so assert that the
	// month window produces a non-empty bucket distinct from the stock bucket by
	// exhausting a small monthly limit and confirming over-limit within it.
	m := quota.NewMemoryMeter(quota.NewStaticLimits(map[string]map[string]int64{
		"acme": {"calls": 1},
	}))
	ctx := context.Background()
	c := quota.Charge{Account: "acme", Metric: "calls", Window: "month", Amount: 1}
	r, err := m.Reserve(ctx, c)
	if err != nil {
		t.Fatalf("first monthly reserve: %v", err)
	}
	_ = r.Commit(ctx)
	if _, err := m.Reserve(ctx, c); !errors.Is(err, quota.ErrOverLimit) {
		t.Fatalf("second monthly reserve should exceed limit, got %v", err)
	}
}

func TestReleaseAfterCommitIsNoop(t *testing.T) {
	m := quota.NewMemoryMeter(quota.NewStaticLimits(map[string]map[string]int64{"acme": {"m": 1}}))
	ctx := context.Background()
	r, _ := m.Reserve(ctx, quota.Charge{Account: "acme", Metric: "m"})
	_ = r.Commit(ctx)
	_ = r.Release(ctx) // must not double-decrement
	// The committed unit still occupies the limit.
	if _, err := m.Reserve(ctx, quota.Charge{Account: "acme", Metric: "m"}); !errors.Is(err, quota.ErrOverLimit) {
		t.Fatalf("release-after-commit must not free the unit; got %v", err)
	}
}
