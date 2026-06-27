package otel_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/server"
)

const probeMethod = "/test.v1.Svc/Do"

var probeServiceDesc = grpc.ServiceDesc{
	ServiceName: "test.v1.Svc",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Do",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(ctx context.Context, req any) (any, error) { return &emptypb.Empty{}, nil }
			if interceptor == nil {
				return h(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: probeMethod}, h)
		},
	}},
}

// TestGatewayGRPCPropagation_SingleTrace proves AC-2: a request through the REST
// gateway produces ONE trace spanning the HTTP server span, the gRPC client span,
// and the gRPC server span — linked by W3C context across the in-process hop.
//
// It installs a real TracerProvider backed by an in-memory exporter + a W3C
// propagator (what the observability/otel adapter does), then drives an HTTP
// request through a server built with the framework's otelhttp/otelgrpc seam.
func TestGatewayGRPCPropagation_SingleTrace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	// Mirror the adapter's global installation so the core's contrib handlers
	// (otelgrpc/otelhttp) record into this provider and propagate W3C context.
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	s, err := server.New(server.Config{
		GRPCAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
		Rules:    []authz.MethodRule{{Method: probeMethod, Public: true}},
		Authorizer: authz.NewDevAuthorizer(authz.Grant{
			Tenant: "*", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*",
		}),
		PrincipalFunc: grpcauthz.DevPrincipalFunc(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	s.GRPCServer().RegisterService(&probeServiceDesc, struct{}{})
	s.RecordMethods(probeMethod)

	// Register a gateway handler that maps GET /do to the probe gRPC method over
	// the in-process client conn (which carries the otelgrpc client stats handler).
	s.RegisterGateway(func(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
		return mux.HandlePath("GET", "/do", func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
			outCtx := metadata.NewOutgoingContext(r.Context(), metadata.Pairs("account-id", "t1", "groups", "admin"))
			if err := conn.Invoke(outCtx, probeMethod, &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	// Wait for the HTTP gateway to bind.
	var httpAddr string
	for i := 0; i < 100; i++ {
		if a := s.HTTPAddr(); a != "" && a != "127.0.0.1:0" {
			httpAddr = a
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if httpAddr == "" {
		t.Fatal("HTTP gateway did not bind")
	}

	resp, err := http.Get("http://" + httpAddr + "/do")
	if err != nil {
		t.Fatalf("GET /do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /do status %d: %s", resp.StatusCode, body)
	}

	// Force flush and collect spans.
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) < 3 {
		t.Fatalf("expected >=3 spans (HTTP server, gRPC client, gRPC server), got %d: %v", len(spans), spanNames(spans))
	}

	// Every span must share ONE trace id (the single end-to-end trace).
	first := spans[0].SpanContext.TraceID()
	for _, sp := range spans {
		if sp.SpanContext.TraceID() != first {
			t.Fatalf("spans span multiple traces: %v has trace %s, want %s (names: %v)",
				sp.Name, sp.SpanContext.TraceID(), first, spanNames(spans))
		}
	}
}

func spanNames(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}
