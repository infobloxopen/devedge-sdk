// Package resilience provides policy interceptors for request timeouts, rate
// limiting, and circuit-breaker seams. Algorithms are behind interfaces so
// callers can swap implementations without changing core.
package resilience

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NoTimeout is a sentinel for ResilienceConfig.RequestTimeout that explicitly
// disables the default 30-second timeout. Set RequestTimeout to NoTimeout (< 0)
// to opt out; 0 triggers the 30s default applied in server.New.
const NoTimeout = time.Duration(-1)

// TimeoutUnary returns a unary server interceptor that bounds each handler
// invocation with a context deadline. The per-method map takes precedence over
// def; a value of 0 in either position disables the timeout for that scope.
// Negative values (e.g. NoTimeout) also disable the timeout.
func TimeoutUnary(def time.Duration, perMethod map[string]time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		d := def
		if v, ok := perMethod[info.FullMethod]; ok {
			d = v
		}
		if d <= 0 {
			return handler(ctx, req)
		}
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		resp, err := handler(ctx, req)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return nil, status.Error(codes.DeadlineExceeded, ctx.Err().Error())
			}
			return nil, err
		}
		return resp, nil
	}
}
