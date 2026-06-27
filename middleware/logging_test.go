package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"

	"github.com/infobloxopen/devedge-sdk/internal/testpb/secretpb"
	"github.com/infobloxopen/devedge-sdk/middleware"
)

// drainRecords decodes the line-delimited JSON slog output captured in buf.
func drainRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal slog record %q: %v", line, err)
		}
		recs = append(recs, m)
	}
	return recs
}

// TestLoggingUnary_RedactsSecretPayload proves the Debug payload log runs the
// request through redact.Message: the secret-annotated field never appears in
// cleartext in any log path (AC-3).
func TestLoggingUnary_RedactsSecretPayload(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	intr := middleware.LoggingUnary(logger)
	req := &secretpb.SecretMsg{Id: "id-1", KeyValue: "sk_live_abc123", PublicValue: "open"}
	handler := func(ctx context.Context, req any) (any, error) {
		return &secretpb.SecretMsg{Id: "id-1", KeyValue: "sk_live_resp", PublicValue: "ok"}, nil
	}
	if _, err := intr(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/svc/Do"}, handler); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	raw := buf.String()
	if strings.Contains(raw, "sk_live_abc123") || strings.Contains(raw, "sk_live_resp") {
		t.Fatalf("secret value leaked into logs:\n%s", raw)
	}
	if !strings.Contains(raw, "[REDACTED]") {
		t.Fatalf("expected redacted payload marker, got:\n%s", raw)
	}
	// The non-secret field is still logged (proves the payload WAS logged, just redacted).
	if !strings.Contains(raw, "id-1") {
		t.Fatalf("expected non-secret field logged, got:\n%s", raw)
	}
}

// TestLoggingUnary_LogsTraceID proves the Info summary carries trace_id/span_id
// pulled from the API span context (AC-3 correlation), and the grpc.code.
func TestLoggingUnary_LogsTraceID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("0102030405060708")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ctx = middleware.WithTenantID(ctx, "tenant-9")

	intr := middleware.LoggingUnary(logger)
	handler := func(ctx context.Context, req any) (any, error) { return &secretpb.SecretMsg{}, nil }
	if _, err := intr(ctx, &secretpb.SecretMsg{}, &grpc.UnaryServerInfo{FullMethod: "/svc/Do"}, handler); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	recs := drainRecords(t, &buf)
	var summary map[string]any
	for _, r := range recs {
		if r["msg"] == "grpc request handled" {
			summary = r
		}
	}
	if summary == nil {
		t.Fatalf("no summary record found in:\n%s", buf.String())
	}
	if got := summary["trace_id"]; got != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %v, want the span context trace id", got)
	}
	if got := summary["span_id"]; got != "0102030405060708" {
		t.Errorf("span_id = %v, want the span context span id", got)
	}
	if got := summary["grpc.code"]; got != "OK" {
		t.Errorf("grpc.code = %v, want OK", got)
	}
	if got := summary["account-id"]; got != "tenant-9" {
		t.Errorf("account-id = %v, want tenant-9", got)
	}
}
