package cells_test

import "google.golang.org/grpc"

// fakeUnaryInfo returns a minimal UnaryServerInfo for tests.
func fakeUnaryInfo() *grpc.UnaryServerInfo {
	return &grpc.UnaryServerInfo{FullMethod: "/svc/CreateFoo"}
}
