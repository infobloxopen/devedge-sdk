package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/server"
)

func TestNew_EmptyGRPCAddr_ReturnsError(t *testing.T) {
	_, err := server.New(server.Config{
		GRPCAddr: "",
	})
	if err == nil {
		t.Fatal("expected error when GRPCAddr is empty, got nil")
	}
}

func TestNew_ValidConfig_Succeeds(t *testing.T) {
	s, err := server.New(server.Config{
		GRPCAddr: ":9090",
	})
	if err != nil {
		t.Fatalf("unexpected error with valid config: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Server, got nil")
	}
}

func TestServe_CancelledContext_ReturnsQuickly(t *testing.T) {
	s, err := server.New(server.Config{
		GRPCAddr: ":0", // ephemeral port
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.Serve(ctx)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned non-nil error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s after context cancellation")
	}
}

// TestAssertMethodsDeclared_MissingRule tests that AssertMethodsDeclared from
// grpcauthz errors when a method has no rule. This is the boot-time gate the
// server should call at startup.
func TestAssertMethodsDeclared_MissingRule_ReturnsError(t *testing.T) {
	methods := []string{
		"/widget.v1.WidgetService/GetWidget",
		"/widget.v1.WidgetService/CreateWidget",
	}
	// Only declare one of the two methods — the other is missing.
	opts := []grpcauthz.Option{
		grpcauthz.WithMethodRule("/widget.v1.WidgetService/GetWidget", authz.Get, "widget"),
	}
	err := grpcauthz.AssertMethodsDeclared(methods, opts...)
	if err == nil {
		t.Fatal("expected error when a method has no declared rule, got nil")
	}
}

// TestServer_Rules_ReturnsConfiguredRules verifies that the server exposes the
// rules it was configured with (needed for the boot-time AssertMethodsDeclared call).
func TestServer_Rules_ReturnsConfiguredRules(t *testing.T) {
	rules := []authz.MethodRule{
		{Method: "/svc.v1.Svc/GetFoo", Verb: authz.Get, Resource: "foo"},
		{Method: "/svc.v1.Svc/CreateFoo", Verb: authz.Create, Resource: "foo", Public: false},
	}
	s, err := server.New(server.Config{
		GRPCAddr: ":9092",
		Rules:    rules,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := s.Rules()
	if len(got) != len(rules) {
		t.Fatalf("expected %d rules, got %d", len(rules), len(got))
	}
	for i, r := range rules {
		if got[i].Method != r.Method || got[i].Verb != r.Verb || got[i].Resource != r.Resource {
			t.Fatalf("rule[%d] mismatch: want %+v, got %+v", i, r, got[i])
		}
	}
}
