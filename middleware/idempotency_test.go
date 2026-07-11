package middleware_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/infobloxopen/devedge-sdk/authz"
	mw "github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

const testMethod = "/toy.v1.WidgetService/CreateWidget"

// idemReq is a test request that is a real proto.Message (via the embedded
// StringValue, so fingerprinting can marshal it) AND carries an AIP-155
// request_id / validate_only. The embedded StringValue's Value is the "body" that
// distinguishes fingerprints.
type idemReq struct {
	*wrapperspb.StringValue
	requestID    string
	validateOnly bool
}

func newReq(id, body string) *idemReq {
	return &idemReq{StringValue: wrapperspb.String(body), requestID: id}
}
func (r *idemReq) GetRequestId() string  { return r.requestID }
func (r *idemReq) GetValidateOnly() bool { return r.validateOnly }

// fakeTxRunner runs fn directly — enough to exercise the interceptor's control
// flow without a real database (the fake store ignores the tx).
type fakeTxRunner struct{}

func (fakeTxRunner) Atomically(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// fakeDurableStore is an in-memory middleware.DurableIdempotencyStore for
// interceptor tests. It records every Claim so a test can assert tenant scoping.
type fakeDurableStore struct {
	mu        sync.Mutex
	m         map[string]persistence.IdempotencyRecord
	claimed   []persistence.IdempotencyKey
	lookupErr error
}

func newFakeStore() *fakeDurableStore {
	return &fakeDurableStore{m: map[string]persistence.IdempotencyRecord{}}
}

func fakeKey(k persistence.IdempotencyKey) string {
	return k.Tenant + "\x00" + k.Method + "\x00" + k.RequestID
}

func (f *fakeDurableStore) Lookup(_ context.Context, key persistence.IdempotencyKey) (persistence.IdempotencyRecord, bool, error) {
	if f.lookupErr != nil {
		return persistence.IdempotencyRecord{}, false, f.lookupErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.m[fakeKey(key)]
	return rec, ok, nil
}

func (f *fakeDurableStore) Claim(_ context.Context, key persistence.IdempotencyKey, fp string, _ time.Duration) (persistence.IdempotencyRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimed = append(f.claimed, key)
	if rec, ok := f.m[fakeKey(key)]; ok {
		return rec, false, nil
	}
	f.m[fakeKey(key)] = persistence.IdempotencyRecord{Status: persistence.StatusInProgress, Fingerprint: fp}
	return persistence.IdempotencyRecord{}, true, nil
}

func (f *fakeDurableStore) Complete(_ context.Context, key persistence.IdempotencyKey, rtype string, resp []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.m[fakeKey(key)] // preserves the fingerprint set at Claim
	rec.Status = persistence.StatusCompleted
	rec.ResponseType = rtype
	rec.Response = resp
	f.m[fakeKey(key)] = rec
	return nil
}

func (f *fakeDurableStore) GC(context.Context, time.Time) (int64, error) { return 0, nil }

func TestDurableDedup_ReplayFromFastPath_SkipsHandler(t *testing.T) {
	store := newFakeStore()
	b, _ := proto.Marshal(wrapperspb.String("server-generated-1"))
	store.m[fakeKey(persistence.IdempotencyKey{Method: testMethod, RequestID: "r1"})] =
		persistence.IdempotencyRecord{Status: persistence.StatusCompleted, ResponseType: "google.protobuf.StringValue", Response: b}

	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("SHOULD-NOT-HAPPEN"), nil }
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	resp, err := intc(context.Background(), newReq("r1", "body"), info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("fast-path replay must not execute the handler, got %d calls", calls)
	}
	if w, ok := resp.(*wrapperspb.StringValue); !ok || w.GetValue() != "server-generated-1" {
		t.Fatalf("expected verbatim replay of server-generated-1, got %#v", resp)
	}
}

func TestDurableDedup_FirstExecutesThenReplays(t *testing.T) {
	store := newFakeStore()
	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("gen-1"), nil }
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}
	req := newReq("r1", "body")

	r1, err := intc(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	r2, err := intc(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler must execute exactly once, got %d", calls)
	}
	if r1.(*wrapperspb.StringValue).GetValue() != "gen-1" || r2.(*wrapperspb.StringValue).GetValue() != "gen-1" {
		t.Fatalf("both responses must be the original gen-1, got %v / %v", r1, r2)
	}
}

func TestDurableDedup_InProgressReturns409(t *testing.T) {
	store := newFakeStore()
	store.m[fakeKey(persistence.IdempotencyKey{Method: testMethod, RequestID: "r1"})] =
		persistence.IdempotencyRecord{Status: persistence.StatusInProgress}
	handler := func(context.Context, any) (any, error) { t.Fatal("handler must not run"); return nil, nil }
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	_, err := intc(context.Background(), newReq("r1", "body"), info, handler)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("in-progress duplicate must be AlreadyExists (409), got %v", err)
	}
}

func TestDurableDedup_TenantScoped(t *testing.T) {
	store := newFakeStore()
	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("x"), nil }
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}
	req := newReq("shared-id", "body")

	ctxA := mw.WithPrincipal(context.Background(), authz.Principal{Tenant: "tenant-A"})
	ctxB := mw.WithPrincipal(context.Background(), authz.Principal{Tenant: "tenant-B"})
	if _, err := intc(ctxA, req, info, handler); err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	if _, err := intc(ctxB, req, info, handler); err != nil {
		t.Fatalf("tenant B: %v", err)
	}
	// Same request_id + method, DIFFERENT tenant → must not collide.
	if calls != 2 {
		t.Fatalf("a request_id reused across tenants must not dedupe, got %d handler calls", calls)
	}
	if len(store.claimed) != 2 || store.claimed[0].Tenant != "tenant-A" || store.claimed[1].Tenant != "tenant-B" {
		t.Fatalf("claim keys must carry the verified tenant, got %+v", store.claimed)
	}
}

func TestDurableDedup_PassThrough(t *testing.T) {
	// validate_only is derived by ValidateOnlyUnary from the request and stashed on
	// ctx; chain it ahead of the durable interceptor exactly as the server does.
	validateOnly := mw.ValidateOnlyUnary()
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	cases := []struct {
		name string
		req  *idemReq
	}{
		{"empty request_id", newReq("", "body")},
		{"validate_only", &idemReq{StringValue: wrapperspb.String("body"), requestID: "r1", validateOnly: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			calls := 0
			inner := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("v"), nil }
			dedup := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
			dedupHandler := func(ctx context.Context, req any) (any, error) { return dedup(ctx, req, info, inner) }

			if _, err := validateOnly(context.Background(), tc.req, info, dedupHandler); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if calls != 1 {
				t.Fatalf("pass-through must call the handler once, got %d", calls)
			}
			if len(store.claimed) != 0 {
				t.Fatalf("pass-through must not touch the store, claimed=%v", store.claimed)
			}
		})
	}
}

func TestDurableDedup_NonProtoResponseFailsLoud(t *testing.T) {
	store := newFakeStore()
	handler := func(context.Context, any) (any, error) { return "not-a-proto", nil }
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	_, err := intc(context.Background(), newReq("r1", "body"), info, handler)
	if status.Code(err) != codes.Internal {
		t.Fatalf("a non-proto response must fail loud (Internal), got %v", err)
	}
}

// TestDurableDedup_FingerprintMismatchRejected also proves fingerprinting is ON
// by default (the config sets no fingerprint field).
func TestDurableDedup_FingerprintMismatchRejected(t *testing.T) {
	store := newFakeStore()
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	alpha := newReq("r1", "alpha")
	beta := newReq("r1", "beta")

	// First body executes and stores its fingerprint.
	calls := 0
	if _, err := intc(context.Background(), alpha, info, func(context.Context, any) (any, error) {
		calls++
		return wrapperspb.String("gen-1"), nil
	}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Same key, DIFFERENT body → reject, handler must not run.
	if _, err := intc(context.Background(), beta, info, func(context.Context, any) (any, error) {
		t.Fatal("handler must not run on a fingerprint mismatch")
		return nil, nil
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("fingerprint mismatch must be InvalidArgument, got %v", err)
	}

	// Same key, SAME body → replay, handler must not run again.
	if _, err := intc(context.Background(), alpha, info, func(context.Context, any) (any, error) {
		t.Fatal("handler must not run on a fingerprint-match replay")
		return nil, nil
	}); err != nil {
		t.Fatalf("same-body replay must succeed, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler must have executed exactly once, got %d", calls)
	}
}

func TestDurableDedup_RequestIDTooLong(t *testing.T) {
	store := newFakeStore()
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	long := strings.Repeat("x", mw.MaxRequestIDLen+1)
	_, err := intc(context.Background(), newReq(long, "body"), info, func(context.Context, any) (any, error) {
		t.Fatal("handler must not run for an over-length request_id")
		return nil, nil
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("over-length request_id must be InvalidArgument, got %v", err)
	}
	if len(store.claimed) != 0 {
		t.Fatalf("over-length request_id must be rejected before the store, claimed=%v", store.claimed)
	}
}

func TestDurableDedup_MaxResponseBytesEnforced(t *testing.T) {
	store := newFakeStore()
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}, MaxResponseBytes: 16})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	big := func(context.Context, any) (any, error) { return wrapperspb.String(strings.Repeat("y", 1024)), nil }
	_, err := intc(context.Background(), newReq("r1", "body"), info, big)
	if status.Code(err) != codes.Internal {
		t.Fatalf("a response over MaxResponseBytes must fail loud (Internal), got %v", err)
	}
}

// TestDurableDedup_LookupErrorFallsThroughToTx covers FM-02: a Lookup error must
// not fail the request — it falls through to the transactional path.
func TestDurableDedup_LookupErrorFallsThroughToTx(t *testing.T) {
	store := newFakeStore()
	store.lookupErr = errors.New("db unavailable")
	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("v"), nil }
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	if _, err := intc(context.Background(), newReq("r1", "body"), info, handler); err != nil {
		t.Fatalf("a Lookup error must fall through, not fail: %v", err)
	}
	if calls != 1 {
		t.Fatalf("fall-through must run the handler once, got %d", calls)
	}
}

// TestDurableDedup_InTxFingerprintAndReplay exercises the IN-TRANSACTION branches
// (idempotency.go: Claim finds an existing completed record): with the fast-path
// Lookup forced to error, a different body is rejected there and the same body
// replays there.
func TestDurableDedup_InTxFingerprintAndReplay(t *testing.T) {
	store := newFakeStore()
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}
	alpha := newReq("r1", "alpha")
	beta := newReq("r1", "beta")

	// Execute alpha normally (stores its fingerprint + response).
	if _, err := intc(context.Background(), alpha, info, func(context.Context, any) (any, error) {
		return wrapperspb.String("gen-1"), nil
	}); err != nil {
		t.Fatalf("alpha: %v", err)
	}

	// Force the fast path to error so the in-tx Claim branch handles the duplicate.
	store.lookupErr = errors.New("forced lookup error")

	// Different body → in-tx fingerprint mismatch.
	if _, err := intc(context.Background(), beta, info, func(context.Context, any) (any, error) {
		t.Fatal("handler must not run on an in-tx fingerprint mismatch")
		return nil, nil
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("in-tx fingerprint mismatch must be InvalidArgument, got %v", err)
	}

	// Same body → in-tx replay of the stored response, handler not run.
	r, err := intc(context.Background(), alpha, info, func(context.Context, any) (any, error) {
		t.Fatal("handler must not run on an in-tx replay")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("in-tx replay: %v", err)
	}
	if r.(*wrapperspb.StringValue).GetValue() != "gen-1" {
		t.Fatalf("in-tx replay must return the original response, got %v", r)
	}
}

func TestDurableDedup_NilResponseFailsLoud(t *testing.T) {
	store := newFakeStore()
	intc := mw.DurableDeduplicateUnary(mw.DurableDedup{Store: store, Tx: fakeTxRunner{}})
	info := &grpc.UnaryServerInfo{FullMethod: testMethod}

	_, err := intc(context.Background(), newReq("r1", "body"), info, func(context.Context, any) (any, error) {
		return nil, nil // a handler bug: no response, no error
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("a nil (non-proto) response must fail loud (Internal), got %v", err)
	}
}
