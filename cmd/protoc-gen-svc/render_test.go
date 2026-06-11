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
	mustContain(t, out, "type WidgetServiceServer interface")
	mustContain(t, out, "CreateWidget(context.Context, *CreateWidgetRequest) (*Widget, error)")
	mustContain(t, out, "GetWidget(context.Context, *GetWidgetRequest) (*Widget, error)")
	mustContain(t, out, "DeleteWidget(context.Context, *DeleteWidgetRequest) (*DeleteWidgetResponse, error)")
	mustContain(t, out, "type UnimplementedWidgetServiceServer struct")
	mustContain(t, out, "RegisterWidgetService(s *grpc.Server, srv WidgetServiceServer)")
	mustContain(t, out, "widgetServiceAdapter")
	mustContain(t, out, "protoc-gen-svc")
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
	// A service with no methods still emits an empty interface.
	mustContain(t, out, "type EmptyServiceServer interface")
}

func mustContain(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected output to contain %q\n--- output ---\n%s", substr, s)
	}
}
