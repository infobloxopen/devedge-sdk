package middleware_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	mw "github.com/infobloxopen/devedge-sdk/middleware"
)

// fakeUpdateReq implements the UpdateMask accessor expected by FieldMaskUnary.
type fakeUpdateReq struct {
	UpdateMask []string
}

func (r *fakeUpdateReq) GetUpdateMask() []string { return r.UpdateMask }

var testVerbMap = map[string]string{
	"/test.v1.Svc/UpdateFoo": "update",
}

func TestFieldMask_UpdateVerbEmptyMask_ReturnsInvalidArgument(t *testing.T) {
	intc := mw.FieldMaskUnary(testVerbMap)
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, nil
	}
	req := &fakeUpdateReq{UpdateMask: nil}
	_, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/UpdateFoo"}, handler)
	if err == nil {
		t.Fatal("expected InvalidArgument error when update-verb method has empty UpdateMask, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("expected codes.InvalidArgument, got %v", st.Code())
	}
}

func TestFieldMask_UpdateVerbNonEmptyMask_PassesThrough(t *testing.T) {
	intc := mw.FieldMaskUnary(testVerbMap)
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}
	req := &fakeUpdateReq{UpdateMask: []string{"name", "description"}}
	resp, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/UpdateFoo"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("expected handler to be called, but it was not")
	}
	if resp != "ok" {
		t.Fatalf("expected handler response 'ok', got %v", resp)
	}
}

func TestFieldMask_NonUpdateVerb_PassesThrough(t *testing.T) {
	intc := mw.FieldMaskUnary(testVerbMap)
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}
	// /test.v1.Svc/GetFoo is not in the verb map, so it's not an update
	req := &fakeUpdateReq{UpdateMask: nil}
	_, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/GetFoo"}, handler)
	if err != nil {
		t.Fatalf("unexpected error for non-update method: %v", err)
	}
	if !called {
		t.Fatal("expected handler to be called for non-update method")
	}
}

// Apply tests use errdetails.ResourceInfo as a convenient flat proto.Message:
// fields are resource_type, resource_name, owner, description (all strings).

func newTestProto() *errdetails.ResourceInfo {
	return &errdetails.ResourceInfo{
		ResourceType: "Widget",
		ResourceName: "widgets/abc",
		Owner:        "testowner",
		Description:  "a test widget",
	}
}

func TestApply_SubsetMask_ClearsOtherFields(t *testing.T) {
	ri := newTestProto()
	mw.Apply(ri, []string{"resource_type"})
	if ri.ResourceType != "Widget" {
		t.Fatalf("resource_type should be retained, got %q", ri.ResourceType)
	}
	if ri.ResourceName != "" {
		t.Fatalf("resource_name should be cleared, got %q", ri.ResourceName)
	}
	if ri.Owner != "" {
		t.Fatalf("owner should be cleared, got %q", ri.Owner)
	}
	if ri.Description != "" {
		t.Fatalf("description should be cleared, got %q", ri.Description)
	}
}

func TestApply_EmptyMask_NoOp(t *testing.T) {
	ri := newTestProto()
	clone := proto.Clone(ri).(*errdetails.ResourceInfo)
	mw.Apply(ri, []string{})
	if !proto.Equal(ri, clone) {
		t.Fatal("Apply with empty mask must not modify the message")
	}
}

func TestApply_JSONNamePath(t *testing.T) {
	ri := newTestProto()
	// "resourceType" is the JSON name for field "resource_type"
	mw.Apply(ri, []string{"resourceType"})
	if ri.ResourceType != "Widget" {
		t.Fatalf("resource_type (via JSON name) should be retained, got %q", ri.ResourceType)
	}
	if ri.ResourceName != "" {
		t.Fatalf("resource_name should be cleared, got %q", ri.ResourceName)
	}
}

func TestApply_UnknownPath_Ignored(t *testing.T) {
	ri := newTestProto()
	mw.Apply(ri, []string{"unknown_xyz", "resource_type"})
	if ri.ResourceType != "Widget" {
		t.Fatalf("resource_type should be retained, got %q", ri.ResourceType)
	}
	if ri.ResourceName != "" {
		t.Fatalf("resource_name should be cleared, got %q", ri.ResourceName)
	}
}

// ReadMaskUnary tests

type fakeReadMaskReq struct {
	mask *fieldmaskpb.FieldMask
}

func (r *fakeReadMaskReq) GetReadMask() *fieldmaskpb.FieldMask { return r.mask }

type fakeNoReadMaskReq struct{}

func TestReadMaskUnary_WithMask_AppliesApply(t *testing.T) {
	intc := mw.ReadMaskUnary()
	ri := newTestProto()
	handler := func(ctx context.Context, req any) (any, error) {
		return ri, nil
	}
	req := &fakeReadMaskReq{mask: &fieldmaskpb.FieldMask{Paths: []string{"resource_type"}}}
	resp, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/GetFoo"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := resp.(*errdetails.ResourceInfo)
	if out.ResourceType != "Widget" {
		t.Fatalf("resource_type should be retained, got %q", out.ResourceType)
	}
	if out.ResourceName != "" {
		t.Fatalf("resource_name should be cleared by read_mask, got %q", out.ResourceName)
	}
}

func TestReadMaskUnary_NoMaskInterface_PassesThrough(t *testing.T) {
	intc := mw.ReadMaskUnary()
	ri := newTestProto()
	handler := func(ctx context.Context, req any) (any, error) {
		return ri, nil
	}
	req := &fakeNoReadMaskReq{}
	resp, err := intc(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/GetFoo"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := resp.(*errdetails.ResourceInfo)
	if out.ResourceName != "widgets/abc" {
		t.Fatalf("resource_name should be unchanged, got %q", out.ResourceName)
	}
}

func TestReadMaskUnary_NilResponse_NoPanic(t *testing.T) {
	intc := mw.ReadMaskUnary()
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, nil
	}
	req := &fakeReadMaskReq{mask: &fieldmaskpb.FieldMask{Paths: []string{"resource_type"}}}
	resp, err := intc(context.Background(), req, &grpc.UnaryServerInfo{}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %v", resp)
	}
}

func TestReadMaskUnary_HandlerError_PassesThrough(t *testing.T) {
	intc := mw.ReadMaskUnary()
	sentinel := errors.New("handler error")
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, sentinel
	}
	req := &fakeReadMaskReq{mask: &fieldmaskpb.FieldMask{Paths: []string{"resource_type"}}}
	_, err := intc(context.Background(), req, &grpc.UnaryServerInfo{}, handler)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error to pass through, got %v", err)
	}
}
