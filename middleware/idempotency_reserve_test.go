package middleware_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	mw "github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// trackTxRunner runs fn directly (no real DB) but records whether a transaction is
// currently open and how many times Atomically was entered — so a test can assert the
// reserve path does NOT hold a transaction across the handler and opens exactly two
// short transactions (reserve + complete).
type trackTxRunner struct {
	active *bool
	depth  *int
}

func (t trackTxRunner) Atomically(ctx context.Context, fn func(context.Context) error) error {
	*t.depth++
	*t.active = true
	defer func() { *t.active = false }()
	return fn(ctx)
}

func reserveIntc(store mw.DurableIdempotencyStore, tx persistence.TxRunner) grpc.UnaryServerInterceptor {
	return mw.DurableReserveUnary(mw.DurableDedup{Store: store, Tx: tx, Mode: mw.DurableModeReserve})
}

func TestReserve_ClaimCommittedBeforeHandler_NotHeldAcross(t *testing.T) {
	store := newFakeStore()
	active := false
	depth := 0
	tx := trackTxRunner{active: &active, depth: &depth}
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	handlerCalls := 0
	handler := func(ctx context.Context, _ any) (any, error) {
		handlerCalls++
		// (1) No DB transaction is held across the remote effect.
		if active {
			t.Error("reserve mode must not hold a transaction across the handler (remote effect)")
		}
		// (2) The reservation is already a COMMITTED in_progress record.
		rec, ok, err := store.Lookup(ctx, persistence.IdempotencyKey{Method: testMethod, RequestID: "r1"})
		if err != nil || !ok {
			t.Errorf("reservation must be committed before the handler runs (ok=%v err=%v)", ok, err)
		} else if rec.Status != persistence.StatusInProgress {
			t.Errorf("reservation must be in_progress before completion, got %q", rec.Status)
		}
		return wrapperspb.String("gen-1"), nil
	}
	intc := reserveIntc(store, tx)
	req := newReq("r1", "body")

	r1, err := intc(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if r1.(*wrapperspb.StringValue).GetValue() != "gen-1" {
		t.Fatalf("expected gen-1, got %v", r1)
	}
	if depth != 2 {
		t.Fatalf("reserve mode must open exactly two short transactions (reserve + complete), got %d", depth)
	}
	// Second call replays verbatim without re-executing.
	r2, err := intc(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if handlerCalls != 1 {
		t.Fatalf("handler (remote effect) must run exactly once, got %d", handlerCalls)
	}
	if r2.(*wrapperspb.StringValue).GetValue() != "gen-1" {
		t.Fatalf("replay must be the original gen-1, got %v", r2)
	}
}

func TestReserve_CommittedInProgressDuplicate_Returns409(t *testing.T) {
	store := newFakeStore()
	store.m[fakeKey(persistence.IdempotencyKey{Method: testMethod, RequestID: "r1"})] =
		persistence.IdempotencyRecord{Status: persistence.StatusInProgress}
	handler := func(context.Context, any) (any, error) { t.Fatal("handler must not run"); return nil, nil }
	active, depth := false, 0
	intc := reserveIntc(store, trackTxRunner{active: &active, depth: &depth})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	_, err := intc(context.Background(), newReq("r1", "body"), info, handler)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a committed in_progress reservation must return AlreadyExists (409), got %v", err)
	}
}

func TestReserve_CompletedReplays(t *testing.T) {
	store := newFakeStore()
	b, _ := marshalStr("server-generated")
	store.m[fakeKey(persistence.IdempotencyKey{Method: testMethod, RequestID: "r1"})] =
		persistence.IdempotencyRecord{Status: persistence.StatusCompleted, ResponseType: "google.protobuf.StringValue", Response: b}
	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("NOPE"), nil }
	active, depth := false, 0
	intc := reserveIntc(store, trackTxRunner{active: &active, depth: &depth})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	resp, err := intc(context.Background(), newReq("r1", "body"), info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("completed replay must not run the handler, got %d", calls)
	}
	if resp.(*wrapperspb.StringValue).GetValue() != "server-generated" {
		t.Fatalf("expected verbatim replay, got %v", resp)
	}
}

func TestReserve_HandlerError_ReleasesReservation(t *testing.T) {
	store := newFakeStore()
	active, depth := false, 0
	intc := reserveIntc(store, trackTxRunner{active: &active, depth: &depth})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}
	req := newReq("r1", "body")

	boom := errors.New("remote failed")
	calls := 0
	failing := func(context.Context, any) (any, error) { calls++; return nil, boom }
	if _, err := intc(context.Background(), req, info, failing); !errors.Is(err, boom) {
		t.Fatalf("handler error must propagate, got %v", err)
	}
	// The reservation was released (Abandon) so nothing is left in_progress.
	if _, ok, _ := store.Lookup(context.Background(), persistence.IdempotencyKey{Method: testMethod, RequestID: "r1"}); ok {
		t.Fatal("a handler error must release the reservation (Abandon), leaving no in_progress record")
	}
	// An immediate retry therefore re-executes and can succeed.
	ok := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("gen-2"), nil }
	r, err := intc(context.Background(), req, info, ok)
	if err != nil {
		t.Fatalf("retry after release must re-execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected the handler to run again after release, total calls=%d", calls)
	}
	if r.(*wrapperspb.StringValue).GetValue() != "gen-2" {
		t.Fatalf("retry response must be gen-2, got %v", r)
	}
}

func TestReserve_CompleteFailure_LeavesInProgress(t *testing.T) {
	store := newFakeStore()
	store.completeErr = errors.New("db down during complete")
	active, depth := false, 0
	intc := reserveIntc(store, trackTxRunner{active: &active, depth: &depth})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}
	req := newReq("r1", "body")

	handler := func(context.Context, any) (any, error) { return wrapperspb.String("gen-1"), nil }
	if _, err := intc(context.Background(), req, info, handler); err == nil {
		t.Fatal("a Complete failure must propagate the error (remote succeeded, record lost)")
	}
	// Documented gap: the reservation stays in_progress, so a duplicate within TTL 409s.
	rec, ok, _ := store.Lookup(context.Background(), persistence.IdempotencyKey{Method: testMethod, RequestID: "r1"})
	if !ok || rec.Status != persistence.StatusInProgress {
		t.Fatalf("Complete failure must leave the reservation in_progress, got ok=%v status=%q", ok, rec.Status)
	}
	store.completeErr = nil
	_, err := intc(context.Background(), req, info, handler)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a duplicate over the stuck in_progress reservation must 409, got %v", err)
	}
}

func TestReserve_FingerprintMismatch(t *testing.T) {
	store := newFakeStore()
	active, depth := false, 0
	intc := reserveIntc(store, trackTxRunner{active: &active, depth: &depth})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	handler := func(context.Context, any) (any, error) { return wrapperspb.String("v"), nil }
	if _, err := intc(context.Background(), newReq("r1", "body-A"), info, handler); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Reuse the key with a DIFFERENT body → fingerprint mismatch (400).
	_, err := intc(context.Background(), newReq("r1", "body-B"), info, handler)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("reused request_id with a different body must be InvalidArgument, got %v", err)
	}
}

func TestReserve_PassThrough(t *testing.T) {
	store := newFakeStore()
	active, depth := false, 0
	intc := reserveIntc(store, trackTxRunner{active: &active, depth: &depth})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}
	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("v"), nil }
	if _, err := intc(context.Background(), newReq("", "body"), info, handler); err != nil {
		t.Fatalf("empty request_id must pass through: %v", err)
	}
	if calls != 1 || len(store.claimed) != 0 {
		t.Fatalf("pass-through must run the handler once and not touch the store (calls=%d claimed=%d)", calls, len(store.claimed))
	}
}

// marshalStr marshals a StringValue for seeding a completed record.
func marshalStr(s string) ([]byte, error) {
	return proto.Marshal(wrapperspb.String(s))
}
