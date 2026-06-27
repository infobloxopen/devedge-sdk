package health_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/health"
)

// staticCheck is a test double whose Check return value is toggled at runtime.
type staticCheck struct {
	name string
	err  error
}

func (c *staticCheck) Name() string              { return c.name }
func (c *staticCheck) Check(_ context.Context) error { return c.err }

// slowCheck sleeps past the aggregator timeout to verify the timeout fires.
type slowCheck struct{}

func (c *slowCheck) Name() string { return "slow" }
func (c *slowCheck) Check(ctx context.Context) error {
	select {
	case <-time.After(10 * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAggregate_NoChecks_ReturnsEmpty(t *testing.T) {
	failures := health.Aggregate(context.Background(), nil)
	if len(failures) != 0 {
		t.Fatalf("expected 0 failures, got %d", len(failures))
	}
}

func TestAggregate_AllPass_ReturnsEmpty(t *testing.T) {
	checks := []health.Check{
		&staticCheck{name: "a", err: nil},
		&staticCheck{name: "b", err: nil},
	}
	failures := health.Aggregate(context.Background(), checks)
	if len(failures) != 0 {
		t.Fatalf("expected 0 failures, got %d", len(failures))
	}
}

func TestAggregate_OneFailure_ReturnsIt(t *testing.T) {
	sentinel := errors.New("db down")
	checks := []health.Check{
		&staticCheck{name: "ok", err: nil},
		&staticCheck{name: "db", err: sentinel},
	}
	failures := health.Aggregate(context.Background(), checks)
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	if failures[0].Name != "db" {
		t.Errorf("failure name = %q, want %q", failures[0].Name, "db")
	}
	if !errors.Is(failures[0].Err, sentinel) {
		t.Errorf("failure err = %v, want sentinel", failures[0].Err)
	}
}

func TestAggregate_SlowCheck_TimesOut(t *testing.T) {
	// The slow check sleeps 10s; the aggregator must time it out at CheckTimeout (2s).
	start := time.Now()
	failures := health.Aggregate(context.Background(), []health.Check{&slowCheck{}})
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Errorf("aggregate took %v; expected ~2s (CheckTimeout) not 10s", elapsed)
	}
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure (timeout), got %d", len(failures))
	}
}

// pingFunc is a test Pinger backed by a function.
type pingFunc func(ctx context.Context) error

func (f pingFunc) PingContext(ctx context.Context) error { return f(ctx) }

func TestDBCheck_NilErr_ReturnsNil(t *testing.T) {
	check := health.NewDBCheck("db", pingFunc(func(_ context.Context) error { return nil }))
	if err := check.Check(context.Background()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestDBCheck_ErrWrapped_ReturnsErr(t *testing.T) {
	sentinel := errors.New("connection refused")
	check := health.NewDBCheck("db", pingFunc(func(_ context.Context) error { return sentinel }))
	err := check.Check(context.Background())
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want to wrap sentinel", err)
	}
}
