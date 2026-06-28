package widgetsv1_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/servicekit"
	"github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1"
)

// TestWidgetServiceModule_Descriptor verifies the generated servicekit.Module
// carries the proto facts (WS-012 P1): module ID from the proto package, the
// service's methods, the generated authz rules, and a module-qualified resource.
func TestWidgetServiceModule_Descriptor(t *testing.T) {
	repo := persistence.NewMemoryRepository[*widgetsv1.Widget, string](func(w *widgetsv1.Widget) string { return w.Id })
	mod := widgetsv1.WidgetServiceModule(widgetsv1.WidgetServiceModuleOptions{Repo: repo})

	d := mod.Descriptor()
	if d.ID != "toy" { // widgets.proto package is "toy.v1" -> module ID "toy"
		t.Errorf("Descriptor.ID = %q, want %q", d.ID, "toy")
	}
	if len(d.Methods) == 0 {
		t.Error("Descriptor.Methods is empty")
	}
	wantMethod := widgetsv1.WidgetService_CreateWidget_FullMethodName
	found := false
	for _, m := range d.Methods {
		if m == wantMethod {
			found = true
		}
	}
	if !found {
		t.Errorf("Descriptor.Methods missing %q", wantMethod)
	}
	// AuthzRules reference the generated table (same source of truth).
	if len(d.AuthzRules) != len(widgetsv1.WidgetServiceAuthzRules) {
		t.Errorf("Descriptor.AuthzRules len = %d, want %d", len(d.AuthzRules), len(widgetsv1.WidgetServiceAuthzRules))
	}
	if len(d.Resources) != 1 || d.Resources[0].Name != "toy.widget" {
		t.Errorf("Descriptor.Resources = %+v, want one {Name: toy.widget}", d.Resources)
	}
}

// TestWidgetServiceModule_RunServes proves the standalone single-module path: the
// generated Module, handed to servicekit.Run, builds the shared server, passes
// the boot gate, serves, and a real gRPC CreateWidget round-trips through the
// generated CRUD handler — the same behavior as registering by hand.
func TestWidgetServiceModule_RunServes(t *testing.T) {
	repo := persistence.NewMemoryRepository[*widgetsv1.Widget, string](func(w *widgetsv1.Widget) string { return w.Id })
	// Pre-seed so CreateWidget can rely on the caller-supplied id (the generated
	// CRUD Create delegates straight to repo.Create).
	mod := widgetsv1.WidgetServiceModule(widgetsv1.WidgetServiceModuleOptions{Repo: repo})

	// A dev authorizer that grants the admin group everything; paired with the dev
	// principal func the client below sends matching metadata.
	authorizer := authz.NewDevAuthorizer(authz.Grant{
		Tenant: "*", Subjects: []string{"group:admin"}, Verbs: []authz.Verb{"*"}, Resource: "*",
	})

	// Run on a kernel-assigned port; we need the bound addr, so build the server
	// indirectly is not exposed — instead drive Run in a goroutine and dial via a
	// fixed ephemeral addr we control. servicekit.Run owns server.New, so we use a
	// concrete loopback port discovered by binding :0 through the server. To keep
	// the test hermetic we use the lower-level path: Run with a real addr.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a fixed-but-unlikely-used loopback port via :0 is not retrievable from
	// Run; so run the module through a server we can observe. The contract we are
	// testing (Module wiring) is identical, so we assert the served round-trip on
	// an addr Run binds. We discover a free port first.
	addr := freeLoopbackAddr(t)

	done := make(chan error, 1)
	go func() {
		done <- servicekit.Run(servicekit.HostConfig{
			Modules:       []servicekit.Module{mod},
			GRPCAddr:      addr,
			Authorizer:    authorizer,
			PrincipalFunc: grpcauthz.DevPrincipalFunc(),
			Context:       ctx,
		})
	}()

	// Wait for the listener to come up, then round-trip a CreateWidget.
	conn := dialWithRetry(t, addr)
	defer conn.Close()
	client := widgetsv1.NewWidgetServiceClient(conn)

	md := metadata.New(map[string]string{"account-id": "t1", "subject": "u1", "groups": "admin"})
	rctx := metadata.NewOutgoingContext(context.Background(), md)
	w, err := client.CreateWidget(rctx, &widgetsv1.CreateWidgetRequest{Widget: &widgetsv1.Widget{Id: "w1", Name: "first"}})
	if err != nil {
		t.Fatalf("CreateWidget via Module-run server: %v", err)
	}
	if w.GetId() != "w1" {
		t.Errorf("created widget id = %q, want w1", w.GetId())
	}

	got, err := client.GetWidget(rctx, &widgetsv1.GetWidgetRequest{Id: "w1"})
	if err != nil {
		t.Fatalf("GetWidget via Module-run server: %v", err)
	}
	if got.GetName() != "first" {
		t.Errorf("got widget name = %q, want first", got.GetName())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("servicekit.Run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("servicekit.Run did not return within 10s after cancel")
	}
}

// freeLoopbackAddr binds :0 on loopback, reads the assigned port, closes the
// listener, and returns the addr for servicekit.Run to bind. A brief race window
// exists between close and re-bind; acceptable for a hermetic unit test.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

// dialWithRetry dials addr and probes until the server is serving (any reply,
// including NotFound, proves the listener is up), then returns the conn.
func dialWithRetry(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		c := widgetsv1.NewWidgetServiceClient(conn)
		md := metadata.New(map[string]string{"account-id": "t1", "subject": "u1", "groups": "admin"})
		_, perr := c.GetWidget(metadata.NewOutgoingContext(ctx, md), &widgetsv1.GetWidgetRequest{Id: "probe"})
		cancel()
		// Unavailable means the listener isn't up yet; anything else (incl.
		// NotFound) proves the server is serving.
		if status.Code(perr) != codes.Unavailable {
			return conn
		}
		time.Sleep(50 * time.Millisecond)
	}
	return conn
}
