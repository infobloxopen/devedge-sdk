package middleware_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mw "github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

func runErrorMapper(t *testing.T, handlerErr error) error {
	t.Helper()
	intc := mw.ErrorMapperUnary()
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, handlerErr
	}
	_, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Svc/Get"}, handler)
	return err
}

func TestErrorMapper_NotFound_MapsToCodesNotFound(t *testing.T) {
	err := runErrorMapper(t, persistence.ErrNotFound)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Fatalf("ErrNotFound: expected codes.NotFound, got %v", st.Code())
	}
}

func TestErrorMapper_Conflict_MapsToAlreadyExists(t *testing.T) {
	err := runErrorMapper(t, persistence.ErrConflict)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.AlreadyExists {
		t.Fatalf("ErrConflict: expected codes.AlreadyExists, got %v", st.Code())
	}
}

func TestErrorMapper_PreconditionFailed_MapsToFailedPrecondition(t *testing.T) {
	err := runErrorMapper(t, persistence.ErrPreconditionFailed)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("ErrPreconditionFailed: expected codes.FailedPrecondition, got %v", st.Code())
	}
}

func TestErrorMapper_StatusMessage_DoesNotContainPersistencePrefix(t *testing.T) {
	cases := []struct {
		name    string
		srcErr  error
		wantCode codes.Code
	}{
		{"not found", persistence.ErrNotFound, codes.NotFound},
		{"conflict", persistence.ErrConflict, codes.AlreadyExists},
		{"precondition", persistence.ErrPreconditionFailed, codes.FailedPrecondition},
		{"no transaction", persistence.ErrNoTransaction, codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runErrorMapper(t, tc.srcErr)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("expected gRPC status error, got: %v", err)
			}
			if strings.Contains(st.Message(), "persistence:") {
				t.Fatalf("status message must not contain 'persistence:' prefix, got: %q", st.Message())
			}
		})
	}
}

// TestErrorMapper_NoTransaction_MapsToInternal verifies the F030/F032 "tx not
// propagated" guard (persistence.ErrNoTransaction) becomes a clean codes.Internal
// rather than leaking the raw "persistence: no transaction on context" string as
// codes.Unknown. It is a server wiring bug, not a client-actionable error.
func TestErrorMapper_NoTransaction_MapsToInternal(t *testing.T) {
	err := runErrorMapper(t, persistence.ErrNoTransaction)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Fatalf("ErrNoTransaction: expected codes.Internal, got %v", st.Code())
	}
	if strings.Contains(st.Message(), "persistence:") || strings.Contains(st.Message(), "transaction") {
		t.Fatalf("Internal status message must not leak the persistence detail, got: %q", st.Message())
	}
}

func TestErrorMapper_UnmappedError_PassesThrough(t *testing.T) {
	sentinel := errors.New("some other error")
	err := runErrorMapper(t, sentinel)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// An unmapped error should pass through as-is (not wrapped in a status).
	// If the mapper wraps it as Internal that is also acceptable; what matters
	// is that it is NOT silently swallowed and NOT misclassified as NotFound/etc.
	st, ok := status.FromError(err)
	if ok {
		// If it became a status, it must NOT be NotFound/AlreadyExists/FailedPrecondition.
		bad := []codes.Code{codes.NotFound, codes.AlreadyExists, codes.FailedPrecondition}
		for _, c := range bad {
			if st.Code() == c {
				t.Fatalf("unmapped error must not be classified as %v", c)
			}
		}
	} else {
		// Passed through as the original error — correct.
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected original sentinel error, got %v", err)
		}
	}
}

// AIP-193 detail tests

func TestErrorMapper_NotFound_HasResourceInfoDetail(t *testing.T) {
	err := runErrorMapper(t, persistence.ErrNotFound)
	st, _ := status.FromError(err)
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.ResourceInfo); ok {
			if ri.Description != "resource not found" {
				t.Fatalf("ResourceInfo.Description: got %q, want %q", ri.Description, "resource not found")
			}
			return
		}
	}
	t.Fatal("expected ResourceInfo detail on ErrNotFound, found none")
}

func TestErrorMapper_Conflict_HasErrorInfoDetail(t *testing.T) {
	err := runErrorMapper(t, persistence.ErrConflict)
	st, _ := status.FromError(err)
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			if ei.Reason != "ALREADY_EXISTS" {
				t.Fatalf("ErrorInfo.Reason: got %q, want %q", ei.Reason, "ALREADY_EXISTS")
			}
			return
		}
	}
	t.Fatal("expected ErrorInfo detail on ErrConflict, found none")
}

func TestErrorMapper_PreconditionFailed_HasErrorInfoDetail(t *testing.T) {
	err := runErrorMapper(t, persistence.ErrPreconditionFailed)
	st, _ := status.FromError(err)
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			if ei.Reason != "PRECONDITION_FAILED" {
				t.Fatalf("ErrorInfo.Reason: got %q, want %q", ei.Reason, "PRECONDITION_FAILED")
			}
			return
		}
	}
	t.Fatal("expected ErrorInfo detail on ErrPreconditionFailed, found none")
}

func TestErrorMapper_FieldViolation_ReturnsBadRequestDetail(t *testing.T) {
	fv := persistence.NewFieldViolation("color", "must be a hex code")
	err := runErrorMapper(t, fv)
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("expected codes.InvalidArgument, got %v", st.Code())
	}
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			if len(br.FieldViolations) != 1 {
				t.Fatalf("expected 1 FieldViolation, got %d", len(br.FieldViolations))
			}
			fvd := br.FieldViolations[0]
			if fvd.Field != "color" {
				t.Fatalf("FieldViolation.Field: got %q, want %q", fvd.Field, "color")
			}
			if fvd.Description != "must be a hex code" {
				t.Fatalf("FieldViolation.Description: got %q, want %q", fvd.Description, "must be a hex code")
			}
			return
		}
	}
	t.Fatal("expected BadRequest detail on FieldViolationError, found none")
}

func TestErrorMapper_WrappedFieldViolation_Unwraps(t *testing.T) {
	fv := persistence.NewFieldViolation("weight", "must be positive")
	wrapped := fmt.Errorf("outer: %w", fv)
	err := runErrorMapper(t, wrapped)
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("expected codes.InvalidArgument, got %v", st.Code())
	}
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			if len(br.FieldViolations) == 1 && br.FieldViolations[0].Field == "weight" {
				return
			}
		}
	}
	t.Fatal("expected BadRequest detail for wrapped FieldViolationError, found none")
}
