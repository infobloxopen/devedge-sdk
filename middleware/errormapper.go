package middleware

import (
	"context"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// ErrorMapperUnary returns a gRPC unary interceptor that maps well-known
// persistence errors to canonical gRPC status codes with AIP-193 detail messages.
func ErrorMapperUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}

		// FieldViolationError is checked first (before ErrNotFound sentinel) because
		// a FieldViolationError wrapping is not an Is-match for the sentinels.
		var fv *persistence.FieldViolationError
		if errors.As(err, &fv) {
			st, detailErr := status.New(codes.InvalidArgument, "invalid argument").WithDetails(
				&errdetails.BadRequest{
					FieldViolations: []*errdetails.BadRequest_FieldViolation{
						{Field: fv.Field, Description: fv.Description},
					},
				},
			)
			if detailErr != nil {
				return nil, status.Error(codes.InvalidArgument, "invalid argument")
			}
			return nil, st.Err()
		}

		switch {
		case errors.Is(err, persistence.ErrNotFound):
			st, detailErr := status.New(codes.NotFound, "not found").WithDetails(
				&errdetails.ResourceInfo{Description: "resource not found"},
			)
			if detailErr != nil {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return nil, st.Err()

		case errors.Is(err, persistence.ErrConflict):
			st, detailErr := status.New(codes.AlreadyExists, "already exists").WithDetails(
				&errdetails.ErrorInfo{Reason: "ALREADY_EXISTS", Domain: "devedge-sdk/persistence"},
			)
			if detailErr != nil {
				return nil, status.Error(codes.AlreadyExists, "already exists")
			}
			return nil, st.Err()

		case errors.Is(err, persistence.ErrPreconditionFailed):
			st, detailErr := status.New(codes.FailedPrecondition, "precondition failed").WithDetails(
				&errdetails.ErrorInfo{Reason: "PRECONDITION_FAILED", Domain: "devedge-sdk/persistence"},
			)
			if detailErr != nil {
				return nil, status.Error(codes.FailedPrecondition, "precondition failed")
			}
			return nil, st.Err()

		default:
			return nil, err
		}
	}
}
