package quota

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
)

// UnaryServerInterceptor enforces declared per-method quotas
// ([authz.MethodRule].Quota, P13) using meter and the live rule set. It is meant
// to run AFTER the authz interceptor (so the authorized principal/tenant is on
// the context) and just before the handler.
//
// For a method with no declared quota it is a passthrough. For a method WITH a
// quota it reserves one unit before the handler, commits on success, and
// releases on a handler error — so a failed operation consumes nothing.
// Over-limit maps to codes.ResourceExhausted. A missing account fails closed
// (codes.Unauthenticated) unless WithFailOpen is set.
func UnaryServerInterceptor(meter Meter, rules func() []authz.MethodRule, opts ...Option) grpc.UnaryServerInterceptor {
	c := newConfig(opts...)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		q := lookupQuota(rules, info.FullMethod)
		if q == nil {
			return handler(ctx, req)
		}
		account := accountFromContext(ctx)
		if account == "" {
			if c.failOpen {
				return handler(ctx, req)
			}
			return nil, status.Error(codes.Unauthenticated, "quota: no account on context")
		}
		res, err := meter.Reserve(ctx, Charge{Account: account, Metric: q.Metric, Window: q.Window, Amount: 1})
		if err != nil {
			if errors.Is(err, ErrOverLimit) {
				return nil, status.Errorf(codes.ResourceExhausted, "quota exceeded for %q", q.Metric)
			}
			if c.failOpen {
				return handler(ctx, req)
			}
			return nil, status.Error(codes.Internal, "quota: meter error")
		}
		resp, herr := handler(ctx, req)
		if herr != nil {
			_ = res.Release(ctx)
			return resp, herr
		}
		// Best-effort commit: the operation already succeeded, so a commit error
		// must not fail the call — durability of the counter is the meter's
		// concern (the dev meter never errors here).
		_ = res.Commit(ctx)
		return resp, nil
	}
}

func lookupQuota(rules func() []authz.MethodRule, fullMethod string) *authz.QuotaRule {
	if rules == nil {
		return nil
	}
	for _, r := range rules() {
		if r.Method == fullMethod {
			return r.Quota
		}
	}
	return nil
}

// accountFromContext resolves the billing account: the authorized principal's
// tenant (stashed by the authz interceptor) preferred, falling back to the
// request's tenant id.
func accountFromContext(ctx context.Context) string {
	if p, ok := middleware.PrincipalFromContext(ctx); ok && p.Tenant != "" {
		return p.Tenant
	}
	return middleware.TenantIDFromContext(ctx)
}

// Option configures the quota interceptor.
type Option func(*config)

type config struct {
	failOpen bool
}

// WithFailOpen lets requests through when the account is missing or the meter
// errors (instead of failing closed). Discouraged; for migration/debugging.
func WithFailOpen(failOpen bool) Option {
	return func(c *config) { c.failOpen = failOpen }
}

func newConfig(opts ...Option) *config {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	return c
}
