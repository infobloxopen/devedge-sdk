package middleware

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/infobloxopen/devedge-sdk/middleware/redact"
)

// LoggingUnary returns a gRPC unary server interceptor that emits one structured
// slog record per RPC, trace-correlated and redaction-on-by-default.
//
// At Info it logs a single summary line per call: method, grpc.code, duration_ms,
// request_id (from RequestIDUnary), and tenant (account-id, from TenantIDUnary).
// When a span is active it also attaches trace_id and span_id pulled from the
// OTel API span context (trace.SpanContextFromContext) — so logs correlate with
// the spans the otelgrpc/otelhttp stats handlers produce. The OTel API is in
// core; with no SDK installed the span context is simply invalid and the
// trace fields are omitted (no overhead, no behavioral change).
//
// At Debug it additionally logs the request and (on success) the response, each
// first run through redact.Message so (infoblox.field.v1.opts).secret = true
// fields are replaced with "[REDACTED]" before they reach any log sink. Payload
// logging is therefore off at Info (summaries only) and redacted wherever it is
// on — redaction is on by default, never opt-in.
//
// A nil logger falls back to slog.Default(). Place this interceptor after
// RequestIDUnary/TenantIDUnary (so request_id and tenant are in context) and
// before the authz interceptor (so the record captures the final code, including
// PermissionDenied).
func LoggingUnary(logger *slog.Logger) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()

		if logger.Enabled(ctx, slog.LevelDebug) {
			if m, ok := req.(proto.Message); ok {
				logger.DebugContext(ctx, "grpc request",
					"method", info.FullMethod,
					"req", redact.Message(m),
				)
			}
		}

		resp, err := handler(ctx, req)

		attrs := []any{
			"method", info.FullMethod,
			"grpc.code", status.Code(err).String(),
			"duration_ms", float64(time.Since(start).Microseconds()) / 1000.0,
		}
		if id := RequestIDFromContext(ctx); id != "" {
			attrs = append(attrs, "request_id", id)
		}
		if tenant := TenantIDFromContext(ctx); tenant != "" {
			attrs = append(attrs, "account-id", tenant)
		}
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
		}
		logger.InfoContext(ctx, "grpc request handled", attrs...)

		if err == nil && logger.Enabled(ctx, slog.LevelDebug) {
			if m, ok := resp.(proto.Message); ok {
				logger.DebugContext(ctx, "grpc response",
					"method", info.FullMethod,
					"resp", redact.Message(m),
				)
			}
		}

		return resp, err
	}
}
