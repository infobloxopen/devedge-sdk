package cells

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/infobloxopen/devedge-sdk/middleware"
)

// defaultRetryAfter is the retry hint attached to a mid-move rejection.
const defaultRetryAfter = 2 * time.Second

// options configures the L1 routing middleware.
type options struct {
	tenantFunc func(context.Context) string
	httpTenant func(*http.Request) string
	isMutating func(fullMethod string) bool
	retryAfter time.Duration
}

// Option configures the L1 routing middleware.
type Option func(*options)

// WithTenantFunc overrides how the tenant is derived from a gRPC context
// (default: middleware.TenantIDFromContext, populated by middleware.TenantIDUnary).
func WithTenantFunc(f func(context.Context) string) Option {
	return func(o *options) {
		if f != nil {
			o.tenantFunc = f
		}
	}
}

// WithHTTPTenantFunc overrides how the tenant is derived from an HTTP request
// (default: the "account-id" header).
func WithHTTPTenantFunc(f func(*http.Request) string) Option {
	return func(o *options) {
		if f != nil {
			o.httpTenant = f
		}
	}
}

// WithMutationPredicate overrides the read-vs-mutation classifier used for
// fail-closed-on-uncertainty (default: AIP method-name prefixes).
func WithMutationPredicate(f func(fullMethod string) bool) Option {
	return func(o *options) {
		if f != nil {
			o.isMutating = f
		}
	}
}

// WithRetryAfter sets the retry hint on mid-move rejections (default 2s).
func WithRetryAfter(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.retryAfter = d
		}
	}
}

func resolveOptions(opts ...Option) options {
	o := options{
		tenantFunc: middleware.TenantIDFromContext,
		httpTenant: func(r *http.Request) string { return r.Header.Get("account-id") },
		isMutating: DefaultIsMutating,
		retryAfter: defaultRetryAfter,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// readPrefixes are the AIP standard read-method name prefixes. A method whose
// local name starts with one of these is classified read-only.
var readPrefixes = []string{"Get", "BatchGet", "List", "Search", "Lookup", "Watch", "Read", "Export", "Query", "Check", "Stream"}

// DefaultIsMutating classifies a gRPC full method as mutating unless its local
// name begins with a known AIP read prefix. Used to fail closed for writes while
// keeping reads available when a tenant's route is uncertain.
func DefaultIsMutating(fullMethod string) bool {
	name := fullMethod
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		name = fullMethod[i+1:]
	}
	for _, p := range readPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// --- admission token context ---

type admissionTokenKey struct{}

func withAdmissionToken(ctx context.Context, tok AdmissionToken) context.Context {
	return context.WithValue(ctx, admissionTokenKey{}, tok)
}

// AdmissionTokenFromContext returns the L2 admission token for the in-flight
// request, if the routing interceptor admitted it.
func AdmissionTokenFromContext(ctx context.Context) (AdmissionToken, bool) {
	t, ok := ctx.Value(admissionTokenKey{}).(AdmissionToken)
	return t, ok
}

// --- metrics (OTel API only; no SDK import — no-op until an adapter installs one) ---

var (
	metricsOnce sync.Once
	rejections  metric.Int64Counter
	gateGauge   metric.Int64UpDownCounter
)

func metrics() (metric.Int64Counter, metric.Int64UpDownCounter) {
	metricsOnce.Do(func() {
		m := otel.Meter("github.com/infobloxopen/devedge-sdk/cells")
		rejections, _ = m.Int64Counter("cells.route.rejections",
			metric.WithDescription("count of requests rejected by cell routing, by reason"))
		gateGauge, _ = m.Int64UpDownCounter("cells.gate.inflight",
			metric.WithDescription("in-flight admissions held by the tenant gate"))
	})
	return rejections, gateGauge
}

func reject(ctx context.Context, reason string) {
	r, _ := metrics()
	if r != nil {
		r.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

// --- gRPC unary interceptor (L1 + L2) ---

// UnaryServerInterceptor returns a gRPC unary interceptor that routes by tenant:
// it resolves the tenant's cell, rejects calls for a moving tenant, defends
// against a stale upstream router (wrong cell), fails closed for writes under
// route uncertainty, and — when gates is non-nil — acquires/releases an L2
// admission token around the handler. A request with no tenant scope passes
// through unchanged. With no routes populated and the default cell, it is a no-op.
func UnaryServerInterceptor(router *Router, gates *GateRegistry, opts ...Option) grpc.UnaryServerInterceptor {
	o := resolveOptions(opts...)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		tenant := o.tenantFunc(ctx)
		if tenant == "" {
			return handler(ctx, req)
		}
		dec := router.Resolve(ctx, tenant)

		if !dec.AdmitNew {
			reject(ctx, "moving")
			return nil, movingErr(dec.State, o.retryAfter)
		}
		if gates != nil && dec.Cell != gates.CellID() {
			reject(ctx, "wrong_cell")
			return nil, status.Error(codes.Unavailable, "tenant not served by this cell")
		}
		if dec.Stale && o.isMutating(info.FullMethod) {
			reject(ctx, "uncertain_write")
			return nil, status.Error(codes.Unavailable, "tenant route uncertain; write rejected")
		}

		if gates != nil {
			tok, err := gates.TryEnter(tenant, dec.RouteEpoch)
			if err != nil {
				reject(ctx, gateReason(err))
				return nil, gateErr(err, o.retryAfter)
			}
			_, g := metrics()
			if g != nil {
				g.Add(ctx, 1)
				defer g.Add(ctx, -1)
			}
			defer gates.Leave(tok)
			ctx = withAdmissionToken(ctx, tok)
		}

		_ = grpc.SetHeader(ctx, metadata.Pairs(
			"cell-id", dec.Cell,
			"x-route-epoch", strconv.FormatUint(dec.RouteEpoch, 10),
		))
		return handler(ctx, req)
	}
}

// StreamServerInterceptor returns the streaming counterpart of
// [UnaryServerInterceptor]. It applies the route/reject/admission decision once at
// stream start and holds the admission token for the stream's lifetime.
func StreamServerInterceptor(router *Router, gates *GateRegistry, opts ...Option) grpc.StreamServerInterceptor {
	o := resolveOptions(opts...)
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		tenant := o.tenantFunc(ctx)
		if tenant == "" {
			return handler(srv, ss)
		}
		dec := router.Resolve(ctx, tenant)

		if !dec.AdmitNew {
			reject(ctx, "moving")
			return movingErr(dec.State, o.retryAfter)
		}
		if gates != nil && dec.Cell != gates.CellID() {
			reject(ctx, "wrong_cell")
			return status.Error(codes.Unavailable, "tenant not served by this cell")
		}
		if dec.Stale && o.isMutating(info.FullMethod) {
			reject(ctx, "uncertain_write")
			return status.Error(codes.Unavailable, "tenant route uncertain; write rejected")
		}

		if gates != nil {
			tok, err := gates.TryEnter(tenant, dec.RouteEpoch)
			if err != nil {
				reject(ctx, gateReason(err))
				return gateErr(err, o.retryAfter)
			}
			_, g := metrics()
			if g != nil {
				g.Add(ctx, 1)
				defer g.Add(ctx, -1)
			}
			defer gates.Leave(tok)
		}
		return handler(srv, ss)
	}
}

// HTTPMiddleware returns L1 routing middleware for an HTTP handler (e.g. the
// grpc-gateway mux or an HTTP-native service). It rejects a moving tenant with
// 503 + Retry-After and fails closed for writes under route uncertainty. Full L2
// admission for HTTP-native handlers is done by calling the gate directly.
func HTTPMiddleware(router *Router, opts ...Option) func(http.Handler) http.Handler {
	o := resolveOptions(opts...)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant := o.httpTenant(r)
			if tenant == "" {
				next.ServeHTTP(w, r)
				return
			}
			dec := router.Resolve(r.Context(), tenant)
			if !dec.AdmitNew {
				reject(r.Context(), "moving")
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(o.retryAfter)))
				http.Error(w, "tenant is migrating; retry later", http.StatusServiceUnavailable)
				return
			}
			if dec.Stale && httpIsMutating(r.Method) {
				reject(r.Context(), "uncertain_write")
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(o.retryAfter)))
				http.Error(w, "tenant route uncertain; write rejected", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("cell-id", dec.Cell)
			next.ServeHTTP(w, r)
		})
	}
}

func httpIsMutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func retryAfterSeconds(d time.Duration) int {
	s := int(d.Seconds())
	if s < 1 {
		s = 1
	}
	return s
}

// --- status mapping ---

// movingErr maps a mid-move rejection to gRPC UNAVAILABLE with a RetryInfo hint
// (transient, retryable — gRPC status guidance).
func movingErr(_ State, retryAfter time.Duration) error {
	return withRetryInfo(codes.Unavailable, "tenant is migrating; retry later", retryAfter)
}

func gateReason(err error) string {
	switch {
	case errors.Is(err, ErrStaleRouteEpoch):
		return "stale_epoch"
	case errors.Is(err, ErrTenantDraining):
		return "draining"
	default:
		return "gate_denied"
	}
}

// gateErr maps a gate admission error to a gRPC status: a stale epoch is a
// fencing conflict (ABORTED); draining is transient (UNAVAILABLE + RetryInfo).
func gateErr(err error, retryAfter time.Duration) error {
	if errors.Is(err, ErrStaleRouteEpoch) {
		return status.Error(codes.Aborted, "stale route epoch; retry against current route")
	}
	return withRetryInfo(codes.Unavailable, "tenant is draining; retry later", retryAfter)
}

func withRetryInfo(code codes.Code, msg string, retryAfter time.Duration) error {
	st := status.New(code, msg)
	if d, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(retryAfter)}); err == nil {
		return d.Err()
	}
	return st.Err()
}
