package main

import (
	"strings"
	"testing"
)

// T001: unit tests for renderSvcFile — pure function, no protogen/buf needed.

func TestRenderSvcFile_basic(t *testing.T) {
	svc := serviceInfo{
		ServiceName: "WidgetService",
		Methods: []methodInfo{
			{Name: "CreateWidget", InputGoIdent: "CreateWidgetRequest", OutputGoIdent: "Widget"},
			{Name: "GetWidget", InputGoIdent: "GetWidgetRequest", OutputGoIdent: "Widget"},
			{Name: "DeleteWidget", InputGoIdent: "DeleteWidgetRequest", OutputGoIdent: "DeleteWidgetResponse"},
		},
	}
	out := renderSvcFile("widgetsv1", "github.com/example/widgets/v1;widgetsv1", []serviceInfo{svc})

	mustContain(t, out, "DO NOT EDIT")
	mustContain(t, out, "package widgetsv1")
	mustContain(t, out, "protoc-gen-svc")

	// protoc-gen-svc no longer re-declares the server interface or unimplemented
	// stub — those are provided by protoc-gen-go-grpc (_grpc.pb.go).
	mustNotContain(t, out, "type WidgetServiceServer interface")
	mustNotContain(t, out, "type UnimplementedWidgetServiceServer struct")

	// Register<Svc> accepts *server.Server and wires gRPC + HTTP gateway.
	mustContain(t, out, "RegisterWidgetService(s *server.Server, srv WidgetServiceServer) error")
	// Boot-gate: fails closed if any method lacks an authz declaration.
	mustContain(t, out, "grpcauthz.AssertMethodsDeclared(")
	mustContain(t, out, "WidgetService_CreateWidget_FullMethodName")
	mustContain(t, out, "WidgetService_GetWidget_FullMethodName")
	mustContain(t, out, "WidgetService_DeleteWidget_FullMethodName")
	// Delegates to grpc-generated server registration.
	mustContain(t, out, "RegisterWidgetServiceServer(s.GRPCServer(), srv)")
	// Wires HTTP gateway via RegisterGateway.
	mustContain(t, out, "RegisterWidgetServiceHandlerClient(ctx, mux, NewWidgetServiceClient(conn))")
}

func TestRenderSvcFile_noServices(t *testing.T) {
	out := renderSvcFile("emptypkg", "example/emptypkg;emptypkg", nil)
	if out != "" {
		t.Fatalf("expected empty output for no services, got:\n%s", out)
	}
}

func TestRenderSvcFile_emptyMethodList(t *testing.T) {
	svc := serviceInfo{ServiceName: "EmptyService", Methods: nil}
	out := renderSvcFile("pkg", "example/pkg;pkg", []serviceInfo{svc})
	// A service with no methods still emits a Register helper.
	mustContain(t, out, "RegisterEmptyService(s *server.Server, srv EmptyServiceServer) error")
	mustContain(t, out, "RegisterEmptyServiceServer(s.GRPCServer(), srv)")
}

func mustContain(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected output to contain %q\n--- output ---\n%s", substr, s)
	}
}

func mustNotContain(t *testing.T, s, substr string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("expected output NOT to contain %q\n--- output ---\n%s", substr, s)
	}
}
