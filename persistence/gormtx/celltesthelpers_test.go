package gormtx_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/infobloxopen/devedge-sdk/cells"
)

// admittedContext returns a context carrying a real cells.AdmissionToken for
// (cellID, tenantID, routeEpoch), produced by the cells routing interceptor — the
// ONLY producer of an admission token. It seeds a MemTable route placing the tenant
// on cellID at routeEpoch, runs the unary interceptor with a GateRegistry for cellID,
// and captures the handler's context (which the interceptor stamped with the token).
//
// This exercises the real cell-routed write path the L3 write-guard enforces against,
// without touching the frozen cells package internals.
func admittedContext(t *testing.T, cellID, tenantID string, routeEpoch uint64) context.Context {
	t.Helper()
	tbl := cells.NewMemTable()
	router := cells.NewRouter(tbl)
	bg, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := router.Start(bg); err != nil {
		t.Fatalf("router.Start: %v", err)
	}
	if err := tbl.CompareAndSet(context.Background(), cells.TenantRoute{}, cells.TenantRoute{
		TenantID:   tenantID,
		RouteEpoch: routeEpoch,
		ActiveCell: cellID,
		State:      cells.StateActive,
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	// Wait for the router's watch cache to observe the route before admitting.
	deadline := time.Now().Add(2 * time.Second)
	for {
		d := router.Resolve(context.Background(), tenantID)
		if d.Known && d.Cell == cellID && d.RouteEpoch == routeEpoch {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("router did not resolve %q to %s@%d within 2s (got %+v)", tenantID, cellID, routeEpoch, d)
		}
		time.Sleep(5 * time.Millisecond)
	}

	gr := cells.NewGateRegistry(cellID, "inst-test")
	interceptor := cells.UnaryServerInterceptor(router, gr,
		cells.WithTenantFunc(func(context.Context) string { return tenantID }),
	)
	var captured context.Context
	handler := func(hctx context.Context, _ any) (any, error) {
		captured = hctx
		return "ok", nil
	}
	if _, err := interceptor(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: "/svc/Write"}, handler); err != nil {
		t.Fatalf("interceptor did not admit %q on %s@%d: %v", tenantID, cellID, routeEpoch, err)
	}
	if _, ok := cells.AdmissionTokenFromContext(captured); !ok {
		t.Fatalf("interceptor did not stamp an admission token for %q", tenantID)
	}
	return captured
}
