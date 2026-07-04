package server_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/infobloxopen/devedge-sdk/authn"
	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/server"
)

// TestNew_Authenticator_VerifiesBeforeAuthz proves the WS-026 server.Config
// .Authenticator wiring: the authentication interceptor runs before authz, the
// authorizer sees the TOKEN-VERIFIED principal (not raw metadata), and the stage
// is fail-closed. It uses a stub Authenticator (the JOSE round-trip is covered in
// authn/oidc) mapping bearer -> principal:
//
//	"alice" -> {tenant t1, group admin}   (granted)
//	"bob"   -> {tenant t2, group viewer}  (not granted)
//	anything else -> error                (rejected Unauthenticated)
func TestNew_Authenticator_VerifiesBeforeAuthz(t *testing.T) {
	auth := authn.AuthenticatorFunc(func(_ context.Context, bearer string) (authz.Principal, error) {
		switch bearer {
		case "alice":
			return authz.Principal{Subject: "alice", Tenant: "t1", Groups: []string{"admin"}}, nil
		case "bob":
			return authz.Principal{Subject: "bob", Tenant: "t2", Groups: []string{"viewer"}}, nil
		default:
			return authz.Principal{}, errors.New("invalid bearer")
		}
	})

	s, err := server.New(server.Config{
		GRPCAddr: ":0",
		Rules:    []authz.MethodRule{{Method: probeMethod, Verb: authz.Get, Resource: "thing"}},
		Authorizer: authz.NewDevAuthorizer(authz.Grant{
			Tenant: "t1", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*",
		}),
		Authenticator: auth, // PrincipalFunc defaults to authn.VerifiedPrincipal
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.GRPCServer().RegisterService(&probeServiceDesc, struct{}{})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.GRPCServer().Serve(lis) }()
	defer s.GRPCServer().Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	call := func(md metadata.MD) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if md != nil {
			ctx = metadata.NewOutgoingContext(ctx, md)
		}
		return conn.Invoke(ctx, probeMethod, &emptypb.Empty{}, &emptypb.Empty{})
	}

	// (1) Valid bearer for a granted identity -> allowed.
	if err := call(metadata.Pairs("authorization", "Bearer alice")); err != nil {
		t.Fatalf("alice (granted) denied: %v", err)
	}

	// (2) No bearer -> empty verified principal -> default-deny (fail closed).
	if err := call(nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no bearer: want PermissionDenied, got %v", err)
	}

	// (3) Invalid bearer -> rejected at authn before authz (fail closed).
	if err := call(metadata.Pairs("authorization", "Bearer garbage")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalid bearer: want Unauthenticated, got %v", err)
	}

	// (4) Valid bearer for a NON-granted identity -> authn passes, authz denies.
	if err := call(metadata.Pairs("authorization", "Bearer bob")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob (verified but ungranted): want PermissionDenied, got %v", err)
	}

	// (5) Spoofed identity headers WITHOUT a valid bearer are NOT trusted: the
	// verified principal is authoritative, so this must still be denied (this is
	// the whole point of replacing DevPrincipalFunc on the trusted path).
	if err := call(metadata.Pairs("account-id", "t1", "groups", "admin")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("spoofed headers, no bearer: want PermissionDenied, got %v", err)
	}
}
