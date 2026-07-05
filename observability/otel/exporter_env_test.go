package otel_test

import (
	"context"
	"testing"

	sdkotel "github.com/infobloxopen/devedge-sdk/observability/otel"
)

// TestSetup_ExporterEnvFallback is the acceptance test for issue #110: when
// Config.Exporter is empty, Setup must consult OTEL_TRACES_EXPORTER before
// defaulting to "otlp", so a developer can flip to a local exporter without a
// source change.
func TestSetup_ExporterEnvFallback(t *testing.T) {
	t.Run("empty config falls back to env none", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "none")

		shutdown, err := sdkotel.Setup(context.Background(), sdkotel.Config{})
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		if shutdown == nil {
			t.Fatal("Setup returned a nil shutdown")
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	})

	t.Run("explicit config wins over env", func(t *testing.T) {
		// Even though the env says "otlp" (which would try to dial a
		// collector), the explicit Config.Exporter="none" must win.
		t.Setenv("OTEL_TRACES_EXPORTER", "otlp")

		shutdown, err := sdkotel.Setup(context.Background(), sdkotel.Config{Exporter: "none"})
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		if shutdown == nil {
			t.Fatal("Setup returned a nil shutdown")
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	})

	t.Run("console alias maps to stdout", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "console")

		shutdown, err := sdkotel.Setup(context.Background(), sdkotel.Config{})
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	})

	t.Run("unknown env value errors", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "bogus")

		if _, err := sdkotel.Setup(context.Background(), sdkotel.Config{}); err == nil {
			t.Fatal("Setup did not error on an unrecognized OTEL_TRACES_EXPORTER value")
		}
	})
}
