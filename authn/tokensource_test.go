package authn_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authn"
)

// staticTokenSource is a fixed TokenSource: it returns token for any audience,
// recording the audiences it was asked for.
type staticTokenSource struct {
	token string
	err   error
	asked []string
}

func (s *staticTokenSource) TokenFor(_ context.Context, targetAudience string) (string, error) {
	s.asked = append(s.asked, targetAudience)
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

func TestStaticAudiences_AudienceFor(t *testing.T) {
	r := authn.StaticAudiences{"orders.v1.Orders": "orders-api"}
	if aud, ok := r.AudienceFor("orders.v1.Orders"); !ok || aud != "orders-api" {
		t.Fatalf("AudienceFor mapped = %q,%v", aud, ok)
	}
	if _, ok := r.AudienceFor("unknown.Svc"); ok {
		t.Fatal("AudienceFor should report false for an unmapped target")
	}
}

func TestUnaryClientInterceptor_AttachesBearer(t *testing.T) {
	ts := &staticTokenSource{token: "scoped-token"}
	r := authn.StaticAudiences{"orders.v1.Orders": "orders-api"}
	interceptor := authn.UnaryClientInterceptor(ts, r)

	var gotAuth string
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		if vals := md.Get("authorization"); len(vals) > 0 {
			gotAuth = vals[0]
		}
		return nil
	}
	err := interceptor(context.Background(), "/orders.v1.Orders/GetOrder", nil, nil, nil, invoker)
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if gotAuth != "Bearer scoped-token" {
		t.Fatalf("authorization metadata = %q, want %q", gotAuth, "Bearer scoped-token")
	}
	if len(ts.asked) != 1 || ts.asked[0] != "orders-api" {
		t.Fatalf("TokenSource asked for %v, want [orders-api]", ts.asked)
	}
}

func TestUnaryClientInterceptor_UnmappedTarget_FailsClosed(t *testing.T) {
	ts := &staticTokenSource{token: "scoped-token"}
	r := authn.StaticAudiences{"orders.v1.Orders": "orders-api"}
	interceptor := authn.UnaryClientInterceptor(ts, r)

	called := false
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		called = true
		return nil
	}
	err := interceptor(context.Background(), "/billing.v1.Billing/Charge", nil, nil, nil, invoker)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unmapped target should fail closed with FailedPrecondition, got %v", err)
	}
	if called {
		t.Fatal("invoker ran for an unmapped target (raw token could leak cross-domain)")
	}
	if len(ts.asked) != 0 {
		t.Fatalf("TokenSource should not be consulted for an unmapped target, asked %v", ts.asked)
	}
}

func TestUnaryClientInterceptor_TokenError_FailsClosed(t *testing.T) {
	ts := &staticTokenSource{err: errors.New("sts down")}
	r := authn.StaticAudiences{"orders.v1.Orders": "orders-api"}
	interceptor := authn.UnaryClientInterceptor(ts, r)

	called := false
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		called = true
		return nil
	}
	err := interceptor(context.Background(), "/orders.v1.Orders/GetOrder", nil, nil, nil, invoker)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("token error should fail closed with Unauthenticated, got %v", err)
	}
	if called {
		t.Fatal("invoker ran despite a token error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewRoundTripper_AttachesBearer(t *testing.T) {
	ts := &staticTokenSource{token: "rest-token"}
	var gotAuth string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})
	rt := authn.NewRoundTripper(base, ts, "reports-api")

	// The original request must not be mutated by the RoundTripper.
	req := httptest.NewRequest(http.MethodGet, "https://reports.dev.test/v1/report", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer rest-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer rest-token")
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("RoundTripper mutated the caller's original request header")
	}
	if len(ts.asked) != 1 || ts.asked[0] != "reports-api" {
		t.Fatalf("TokenSource asked for %v, want [reports-api]", ts.asked)
	}
}

func TestNewRoundTripper_TokenError_FailsClosed(t *testing.T) {
	ts := &staticTokenSource{err: errors.New("sts down")}
	called := false
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	rt := authn.NewRoundTripper(base, ts, "reports-api")
	req := httptest.NewRequest(http.MethodGet, "https://reports.dev.test/v1/report", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("a token error must fail the request (no unauthenticated send)")
	}
	if called {
		t.Fatal("base transport ran despite a token error")
	}
}
