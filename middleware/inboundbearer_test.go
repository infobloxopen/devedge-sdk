package middleware_test

import (
	"context"
	"testing"

	"github.com/infobloxopen/devedge-sdk/middleware"
)

func TestInboundBearer_RoundTrip(t *testing.T) {
	ctx := middleware.WithInboundBearer(context.Background(), "raw.jwt.value")
	got, ok := middleware.InboundBearerFromContext(ctx)
	if !ok || got != "raw.jwt.value" {
		t.Fatalf("InboundBearerFromContext = %q,%v", got, ok)
	}
}

func TestInboundBearer_Absent(t *testing.T) {
	if got, ok := middleware.InboundBearerFromContext(context.Background()); ok || got != "" {
		t.Fatalf("absent inbound bearer should be \"\",false, got %q,%v", got, ok)
	}
}
