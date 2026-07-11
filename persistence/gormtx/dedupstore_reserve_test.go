package gormtx_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// TestDurableDedupStore_Abandon_GuardedToInProgress proves Abandon deletes an
// in_progress reservation but NEVER a completed record (so a durable response is
// never erased).
func TestDurableDedupStore_Abandon_GuardedToInProgress(t *testing.T) {
	db := openDedupDB(t, "dedup_abandon")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	key := persistence.IdempotencyKey{Tenant: "t1", Method: "m", RequestID: "r1"}

	// Reserve (claim + commit) in its own tx.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, err := store.Claim(ctx, key, "fp", time.Hour)
		if err != nil || !claimed {
			return fmt.Errorf("claim: claimed=%v err=%v", claimed, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// Abandon deletes the in_progress reservation.
	var deleted bool
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		var e error
		deleted, e = store.Abandon(ctx, key)
		return e
	}); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if !deleted {
		t.Fatal("Abandon must report the in_progress row was deleted")
	}
	if _, ok, _ := store.Lookup(context.Background(), key); ok {
		t.Fatal("abandoned reservation must be gone")
	}

	// Re-claim then Complete → a completed record.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		if _, claimed, err := store.Claim(ctx, key, "fp", time.Hour); err != nil || !claimed {
			return fmt.Errorf("re-claim: claimed=%v err=%v", claimed, err)
		}
		return store.Complete(ctx, key, "google.protobuf.StringValue", []byte("resp"))
	}); err != nil {
		t.Fatalf("re-claim+complete: %v", err)
	}
	// Abandon must NOT delete a completed record.
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		var e error
		deleted, e = store.Abandon(ctx, key)
		return e
	}); err != nil {
		t.Fatalf("abandon completed: %v", err)
	}
	if deleted {
		t.Fatal("Abandon must NEVER delete a completed record")
	}
	if rec, ok, _ := store.Lookup(context.Background(), key); !ok || rec.Status != persistence.StatusCompleted {
		t.Fatalf("completed record must survive Abandon, got ok=%v status=%q", ok, rec.Status)
	}
}

// TestDurableReserve_Interceptor_Saga_RealDB drives the reserve→remote→complete saga
// through DurableReserveUnary against a real (SQLite) store: exactly-once handler
// execution + verbatim replay, with the reservation committed before the handler.
func TestDurableReserve_Interceptor_Saga_RealDB(t *testing.T) {
	db := openDedupDB(t, "dedup_reserve_saga")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	intc := middleware.DurableReserveUnary(middleware.DurableDedup{Store: store, Tx: tx, Mode: middleware.DurableModeReserve})
	info := &grpc.UnaryServerInfo{FullMethod: "/toy.v1.WidgetService/CreateWidget"}
	key := persistence.IdempotencyKey{Tenant: "t1", Method: info.FullMethod, RequestID: "r1"}

	remoteCalls := 0
	handler := func(ctx context.Context, _ any) (any, error) {
		remoteCalls++
		// The reservation is a COMMITTED in_progress record while the remote runs.
		if rec, ok, _ := store.Lookup(context.Background(), key); !ok || rec.Status != persistence.StatusInProgress {
			t.Errorf("reservation must be committed in_progress during the remote effect, ok=%v status=%q", ok, rec.Status)
		}
		return wrapperspb.String(fmt.Sprintf("remote-%d", remoteCalls)), nil
	}
	ctx := middleware.WithPrincipal(context.Background(), authz.Principal{Tenant: "t1"})
	req := &idemReq{StringValue: wrapperspb.String("body"), requestID: "r1"}

	r1, err := intc(ctx, req, info, handler)
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	r2, err := intc(ctx, req, info, handler)
	if err != nil {
		t.Fatalf("call 2 (retry): %v", err)
	}
	if remoteCalls != 1 {
		t.Fatalf("remote effect must run exactly once across the retry, ran %d", remoteCalls)
	}
	if r1.(*wrapperspb.StringValue).GetValue() != "remote-1" || r2.(*wrapperspb.StringValue).GetValue() != "remote-1" {
		t.Fatalf("retry must replay the ORIGINAL response, got %v / %v", r1, r2)
	}
}

// TestDurableReserve_Interceptor_HandlerError_ReleasesAndRetries_RealDB proves a
// handler (remote) error releases the reservation so an immediate retry re-executes.
func TestDurableReserve_Interceptor_HandlerError_ReleasesAndRetries_RealDB(t *testing.T) {
	db := openDedupDB(t, "dedup_reserve_err")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	intc := middleware.DurableReserveUnary(middleware.DurableDedup{Store: store, Tx: tx, Mode: middleware.DurableModeReserve})
	info := &grpc.UnaryServerInfo{FullMethod: "/toy.v1.WidgetService/CreateWidget"}
	key := persistence.IdempotencyKey{Tenant: "t1", Method: info.FullMethod, RequestID: "r1"}
	ctx := middleware.WithPrincipal(context.Background(), authz.Principal{Tenant: "t1"})
	req := &idemReq{StringValue: wrapperspb.String("body"), requestID: "r1"}

	boom := errors.New("remote unavailable")
	calls := 0
	failing := func(context.Context, any) (any, error) { calls++; return nil, boom }
	if _, err := intc(ctx, req, info, failing); !errors.Is(err, boom) {
		t.Fatalf("handler error must propagate, got %v", err)
	}
	if _, ok, _ := store.Lookup(context.Background(), key); ok {
		t.Fatal("a handler error must release the reservation (Abandon), no row must remain")
	}
	ok := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("recovered"), nil }
	r, err := intc(ctx, req, info, ok)
	if err != nil {
		t.Fatalf("retry after release: %v", err)
	}
	if calls != 2 || r.(*wrapperspb.StringValue).GetValue() != "recovered" {
		t.Fatalf("retry must re-execute and succeed (calls=%d resp=%v)", calls, r)
	}
}

// TestDurableReserve_Interceptor_CommittedInProgress_409 proves a duplicate arriving
// while a committed reservation is in_progress gets AlreadyExists (409).
func TestDurableReserve_Interceptor_CommittedInProgress_409(t *testing.T) {
	db := openDedupDB(t, "dedup_reserve_409")
	store := gormtx.NewGormDurableDedupStore(db)
	tx := gormtx.NewGormTxRunner(db)
	info := &grpc.UnaryServerInfo{FullMethod: "/toy.v1.WidgetService/CreateWidget"}
	key := persistence.IdempotencyKey{Tenant: "t1", Method: info.FullMethod, RequestID: "r1"}

	// Reserve + commit a bare in_progress record (a saga reservation in flight).
	if err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, err := store.Claim(ctx, key, "", time.Hour)
		if err != nil || !claimed {
			return fmt.Errorf("seed reservation: claimed=%v err=%v", claimed, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	intc := middleware.DurableReserveUnary(middleware.DurableDedup{Store: store, Tx: tx, Mode: middleware.DurableModeReserve})
	ctx := middleware.WithPrincipal(context.Background(), authz.Principal{Tenant: "t1"})
	req := &idemReq{StringValue: wrapperspb.String("body"), requestID: "r1"}
	handler := func(context.Context, any) (any, error) { t.Fatal("handler must not run on a 409"); return nil, nil }

	_, err := intc(ctx, req, info, handler)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate over a committed in_progress reservation must 409, got %v", err)
	}
}
