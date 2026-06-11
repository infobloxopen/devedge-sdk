package middleware_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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
