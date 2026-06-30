package quota_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/quota"
)

const sandboxMethod = "/sandbox.v1.SandboxService/CreateSandbox"

func sandboxRules() []authz.MethodRule {
	return []authz.MethodRule{{
		Method:   sandboxMethod,
		Verb:     authz.Create,
		Resource: "sandbox",
		Quota:    &authz.QuotaRule{Metric: "sandboxes"},
	}}
}

// ctxWithAccount mimics what the authz interceptor stashes upstream.
func ctxWithAccount(account string) context.Context {
	return middleware.WithPrincipal(context.Background(), authz.Principal{Tenant: account})
}

func invoke(ic grpc.UnaryServerInterceptor, ctx context.Context, method string, handler grpc.UnaryHandler) (any, error) {
	return ic(ctx, struct{}{}, &grpc.UnaryServerInfo{FullMethod: method}, handler)
}

func okHandler(_ context.Context, _ any) (any, error) { return "ok", nil }

func TestQuotaInterceptorEnforcesLimit(t *testing.T) {
	meter := quota.NewMemoryMeter(quota.NewStaticLimits(map[string]map[string]int64{
		"acme": {"sandboxes": 1},
	}))
	ic := quota.UnaryServerInterceptor(meter, sandboxRules)
	ctx := ctxWithAccount("acme")

	if _, err := invoke(ic, ctx, sandboxMethod, okHandler); err != nil {
		t.Fatalf("first call should pass: %v", err)
	}
	// Second call exceeds the limit of 1.
	_, err := invoke(ic, ctx, sandboxMethod, okHandler)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
}

func TestQuotaInterceptorReleasesOnHandlerError(t *testing.T) {
	meter := quota.NewMemoryMeter(quota.NewStaticLimits(map[string]map[string]int64{
		"acme": {"sandboxes": 1},
	}))
	ic := quota.UnaryServerInterceptor(meter, sandboxRules)
	ctx := ctxWithAccount("acme")

	boom := func(context.Context, any) (any, error) { return nil, errors.New("handler failed") }
	if _, err := invoke(ic, ctx, sandboxMethod, boom); err == nil {
		t.Fatalf("handler error must surface")
	}
	// The failed op must have consumed nothing — a subsequent good call passes.
	if _, err := invoke(ic, ctx, sandboxMethod, okHandler); err != nil {
		t.Fatalf("reservation should have been released on handler error: %v", err)
	}
}

func TestQuotaInterceptorPassthroughWhenNoQuota(t *testing.T) {
	meter := quota.NewMemoryMeter(quota.NewStaticLimits(nil))
	ic := quota.UnaryServerInterceptor(meter, sandboxRules)
	// A method with no rule/quota is a passthrough even with an empty account.
	if _, err := invoke(ic, context.Background(), "/other.v1.Svc/Get", okHandler); err != nil {
		t.Fatalf("no-quota method must pass through: %v", err)
	}
}

func TestQuotaInterceptorFailsClosedWithoutAccount(t *testing.T) {
	meter := quota.NewMemoryMeter(quota.NewStaticLimits(map[string]map[string]int64{"acme": {"sandboxes": 1}}))
	ic := quota.UnaryServerInterceptor(meter, sandboxRules)
	_, err := invoke(ic, context.Background(), sandboxMethod, okHandler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing account must fail closed (Unauthenticated), got %v", err)
	}

	icOpen := quota.UnaryServerInterceptor(meter, sandboxRules, quota.WithFailOpen(true))
	if _, err := invoke(icOpen, context.Background(), sandboxMethod, okHandler); err != nil {
		t.Fatalf("fail-open must let the call through: %v", err)
	}
}
