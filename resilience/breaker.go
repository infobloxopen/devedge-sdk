package resilience

import (
	"context"

	"google.golang.org/grpc"
)

// CircuitBreaker is the seam for circuit-breaker implementations. Execute runs
// fn; if the breaker is open it should return an error without calling fn.
// Implementations — e.g. sony/gobreaker or afex/hystrix-go — are plugged in
// by consumers; the SDK ships the interface and interceptor only and depends on
// no breaker library itself.
type CircuitBreaker interface {
	Execute(ctx context.Context, fn func() (any, error)) (any, error)
}

// BreakerUnary returns a unary server interceptor that delegates each handler
// invocation to b.Execute. When the breaker is open, Execute's error is returned
// directly. If b is nil the interceptor is a no-op pass-through.
func BreakerUnary(b CircuitBreaker) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if b == nil {
			return handler(ctx, req)
		}
		return b.Execute(ctx, func() (any, error) {
			return handler(ctx, req)
		})
	}
}
