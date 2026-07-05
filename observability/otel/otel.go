// Package otel is the OpenTelemetry adapter for the SDK's observability seam.
//
// CLEAN CORE: this package is the ONLY one in the SDK that imports the OTel SDK
// (go.opentelemetry.io/otel/sdk/...) or any exporter (.../exporters/...). The
// core (server, middleware, authz, persistence, ...) instruments against the OTel
// *API* through the otelgrpc/otelhttp contrib handlers, which call the GLOBAL
// no-op TracerProvider/MeterProvider until Setup installs a real one here. Mirrors
// the events/kafkabus discipline (franz-go confined to one adapter): with no Setup
// call everything is inert — zero overhead, no behavioral change. Verified in CI
// by the cleancore import-guard test.
//
// Setup is the single switch a deployment flips: it installs the global providers
// + a W3C TraceContext/Baggage propagator and returns a bounded shutdown. The core
// then emits spans + RED metrics through the contrib handlers it already wired,
// and middleware.LoggingUnary correlates logs via the API span context.
package otel

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// shutdownTimeout bounds the flush of every exporter on shutdown so a hung
// collector can never block process exit.
const shutdownTimeout = 5 * time.Second

// Config selects the exporter and identifies the service. The zero value is
// valid: it uses the "otlp" exporter driven by the standard OTEL_* environment,
// no-opping cleanly when no endpoint is configured.
type Config struct {
	// ServiceName and ServiceVersion identify this service on every span/metric
	// (OTel resource service.name / service.version). When ServiceName is empty
	// OTEL_SERVICE_NAME (or "unknown_service") applies.
	ServiceName    string
	ServiceVersion string
	// Exporter selects the backend: "otlp" (default), "stdout" (dev), or "none"
	// (no-op providers — fully disable export without removing the call). When
	// Exporter is empty, Setup falls back to the OTEL_TRACES_EXPORTER
	// environment variable (accepting the OpenTelemetry-standard "console"
	// alias for "stdout") before defaulting to "otlp" — see Setup.
	Exporter string
	// OTLPEndpoint optionally overrides OTEL_EXPORTER_OTLP_ENDPOINT for the OTLP
	// exporter (host:port, e.g. "localhost:4317"). Leave empty to honor the env.
	OTLPEndpoint string
}

// Setup installs the global TracerProvider, MeterProvider, and a W3C
// TraceContext+Baggage propagator, then returns a shutdown that flushes the
// exporters within a bounded timeout. The core's otelgrpc/otelhttp handlers and
// the logging interceptor automatically pick up the globals it sets.
//
// The "otlp" exporter honors the standard OTEL_* env vars (endpoint, headers,
// protocol, sampler, ...), so a deployment is configured with no code change. If
// no endpoint is set the exporter is still created (lazily — it never dials at
// construction) and simply has nothing to flush, so an unset endpoint never
// crashes startup; it is a clean no-op-on-export until an endpoint appears.
//
// Exporter selection precedence: an explicit, non-empty cfg.Exporter always
// wins (code beats env). When cfg.Exporter is empty, Setup falls back to the
// OTEL_TRACES_EXPORTER environment variable — accepting the SDK's own
// "otlp"/"stdout"/"none" vocabulary plus the OpenTelemetry-standard "console"
// alias for "stdout" (case-insensitively) — so a developer can flip to stdout
// for local runs without touching code. An unset/empty env still defaults to
// "otlp"; an unrecognized env value is passed through to the switch below and
// rejected there like any other unknown exporter.
//
// Setup never sets globals on the error path: if an exporter fails to construct
// it returns the error with a no-op shutdown, leaving the process on the global
// no-op providers.
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	exporter := cfg.Exporter
	if exporter == "" {
		exporter = strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER")))
		if exporter == "console" {
			exporter = "stdout"
		}
	}
	if exporter == "" {
		exporter = "otlp"
	}

	if exporter == "none" {
		// Explicit no-op: install the W3C propagator (so context still flows for
		// any downstream that DID install an SDK) but keep the global no-op
		// providers. Nothing to shut down.
		otel.SetTextMapPropagator(newPropagator())
		return noop, nil
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		return noop, fmt.Errorf("otel: build resource: %w", err)
	}

	var (
		spanExp  sdktrace.SpanExporter
		metricRd sdkmetric.Reader
	)
	switch exporter {
	case "otlp":
		opts := []otlptracegrpc.Option{}
		mopts := []otlpmetricgrpc.Option{}
		if cfg.OTLPEndpoint != "" {
			// Insecure is appropriate for the plaintext in-cluster collector the
			// scaffold targets; TLS is configured via the standard OTEL_* env.
			opts = append(opts, otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint), otlptracegrpc.WithInsecure())
			mopts = append(mopts, otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint), otlpmetricgrpc.WithInsecure())
		}
		// The OTLP gRPC exporters connect lazily, so an unset/unreachable endpoint
		// does not fail construction or block startup.
		spanExp, err = otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return noop, fmt.Errorf("otel: otlp trace exporter: %w", err)
		}
		mExp, merr := otlpmetricgrpc.New(ctx, mopts...)
		if merr != nil {
			_ = spanExp.Shutdown(ctx)
			return noop, fmt.Errorf("otel: otlp metric exporter: %w", merr)
		}
		metricRd = sdkmetric.NewPeriodicReader(mExp)
	case "stdout":
		spanExp, err = stdouttrace.New()
		if err != nil {
			return noop, fmt.Errorf("otel: stdout trace exporter: %w", err)
		}
		mExp, merr := stdoutmetric.New()
		if merr != nil {
			return noop, fmt.Errorf("otel: stdout metric exporter: %w", merr)
		}
		metricRd = sdkmetric.NewPeriodicReader(mExp)
	default:
		return noop, fmt.Errorf("otel: unknown exporter %q (want \"otlp\", \"stdout\", or \"none\")", exporter)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExp),
		sdktrace.WithResource(res),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricRd),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(newPropagator())

	shutdown = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		var errs []error
		if e := tp.Shutdown(ctx); e != nil {
			errs = append(errs, e)
		}
		if e := mp.Shutdown(ctx); e != nil {
			errs = append(errs, e)
		}
		switch len(errs) {
		case 0:
			return nil
		case 1:
			return errs[0]
		default:
			return fmt.Errorf("otel: shutdown: %v", errs)
		}
	}
	return shutdown, nil
}

// newResource builds the OTel resource from the configured service identity,
// merged over the SDK's environment/process defaults.
func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	var attrs []attribute.KeyValue
	if cfg.ServiceName != "" {
		attrs = append(attrs, semconv.ServiceName(cfg.ServiceName))
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithProcess(),
		resource.WithAttributes(attrs...),
	)
}

// newPropagator returns the W3C TraceContext + Baggage composite propagator that
// links the gateway HTTP span to the gRPC server span across the in-process hop.
func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}
