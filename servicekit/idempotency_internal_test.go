package servicekit

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// --- test doubles -------------------------------------------------------------

type idemReq struct {
	*wrapperspb.StringValue
	requestID string
}

func (r *idemReq) GetRequestId() string { return r.requestID }

func req(id, body string) *idemReq { return &idemReq{StringValue: wrapperspb.String(body), requestID: id} }

// methModule is a minimal Module whose only meaningful facts are its ID + methods
// (used to drive hostDurableDedup.build routing). Register is a no-op.
type methModule struct {
	id      string
	methods []string
}

func (m methModule) Descriptor() Descriptor { return Descriptor{ID: m.id, Methods: m.methods} }
func (m methModule) Register(context.Context, *App) error { return nil }

// errLookupStore wraps a memDurableStore but fails Lookup — models an un-migrated
// idempotency_keys table for the boot probe.
type errLookupStore struct{ *memDurableStore }

func (errLookupStore) Lookup(context.Context, persistence.IdempotencyKey) (persistence.IdempotencyRecord, bool, error) {
	return persistence.IdempotencyRecord{}, false, errors.New("no such table: idempotency_keys")
}

// gcCounter counts GC sweeps for the GC-loop lifecycle test.
type gcCounter struct {
	*memDurableStore
	n int32
}

func (g *gcCounter) GC(context.Context, time.Time) (int64, error) { atomic.AddInt32(&g.n, 1); return 0, nil }

// --- memDurableStore ----------------------------------------------------------

func TestMemDurableStore_ExactlyOnce(t *testing.T) {
	s := newMemDurableStore()
	intc := middleware.DurableDeduplicateUnary(middleware.DurableDedup{Store: s, Tx: s})
	info := &grpc.UnaryServerInfo{FullMethod: "/m.v1.S/Create"}
	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("gen-1"), nil }

	r1, err := intc(context.Background(), req("r1", "b"), info, handler)
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	r2, err := intc(context.Background(), req("r1", "b"), info, handler)
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler must run exactly once, got %d", calls)
	}
	if r1.(*wrapperspb.StringValue).GetValue() != "gen-1" || r2.(*wrapperspb.StringValue).GetValue() != "gen-1" {
		t.Fatalf("both must replay gen-1, got %v / %v", r1, r2)
	}
}

func TestMemDurableStore_RollbackOnHandlerError(t *testing.T) {
	s := newMemDurableStore()
	intc := middleware.DurableDeduplicateUnary(middleware.DurableDedup{Store: s, Tx: s})
	info := &grpc.UnaryServerInfo{FullMethod: "/m.v1.S/Create"}
	boom := errors.New("handler failed")

	calls := 0
	if _, err := intc(context.Background(), req("r1", "b"), info, func(context.Context, any) (any, error) {
		calls++
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("want handler error, got %v", err)
	}
	// The claim must have rolled back → a retry re-executes (errors never cached).
	if _, ok, _ := s.Lookup(context.Background(), persistence.IdempotencyKey{Method: info.FullMethod, RequestID: "r1"}); ok {
		t.Fatal("a failed handler must leave no record (rollback)")
	}
	r, err := intc(context.Background(), req("r1", "b"), info, func(context.Context, any) (any, error) {
		calls++
		return wrapperspb.String("ok"), nil
	})
	if err != nil || calls != 2 || r.(*wrapperspb.StringValue).GetValue() != "ok" {
		t.Fatalf("retry must re-execute and succeed (calls=%d r=%v err=%v)", calls, r, err)
	}
}

func TestMemDurableStore_ReserveSaga(t *testing.T) {
	s := newMemDurableStore()
	intc := middleware.DurableReserveUnary(middleware.DurableDedup{Store: s, Tx: s, Mode: middleware.DurableModeReserve})
	info := &grpc.UnaryServerInfo{FullMethod: "/m.v1.S/DoRemote"}
	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("remote"), nil }

	if _, err := intc(context.Background(), req("r1", "b"), info, handler); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	r2, err := intc(context.Background(), req("r1", "b"), info, handler)
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if calls != 1 || r2.(*wrapperspb.StringValue).GetValue() != "remote" {
		t.Fatalf("reserve saga must run once + replay (calls=%d r=%v)", calls, r2)
	}
}

func TestMemDurableStore_TTLAndGC(t *testing.T) {
	now := time.Now()
	s := newMemDurableStore()
	s.now = func() time.Time { return now }
	key := persistence.IdempotencyKey{Method: "m", RequestID: "r1"}

	if err := s.Atomically(context.Background(), func(ctx context.Context) error {
		_, claimed, err := s.Claim(ctx, key, "", 100*time.Millisecond)
		if err != nil || !claimed {
			return errors.New("claim")
		}
		return s.Complete(ctx, key, "google.protobuf.StringValue", []byte("x"))
	}); err != nil {
		t.Fatalf("claim+complete: %v", err)
	}
	if _, ok, _ := s.Lookup(context.Background(), key); !ok {
		t.Fatal("record must be live before expiry")
	}
	now = now.Add(time.Second) // advance past TTL
	if _, ok, _ := s.Lookup(context.Background(), key); ok {
		t.Fatal("expired record must read as absent")
	}
	n, err := s.GC(context.Background(), now)
	if err != nil || n != 1 {
		t.Fatalf("GC must remove the expired record, got n=%d err=%v", n, err)
	}
}

func TestMemDurableStore_FailLoudOutsideTx(t *testing.T) {
	s := newMemDurableStore()
	key := persistence.IdempotencyKey{Method: "m", RequestID: "r1"}
	if _, _, err := s.Claim(context.Background(), key, "", time.Hour); err == nil || !strings.Contains(err.Error(), "outside Atomically") {
		t.Fatalf("Claim outside Atomically must fail loud, got %v", err)
	}
	if err := s.Complete(context.Background(), key, "t", nil); err == nil {
		t.Fatal("Complete outside Atomically must fail loud")
	}
	if _, err := s.Abandon(context.Background(), key); err == nil {
		t.Fatal("Abandon outside Atomically must fail loud")
	}
}

// --- hostDurableDedup ---------------------------------------------------------

func TestHostDurableDedup_Routing(t *testing.T) {
	h := newHostDurableDedup()
	sA, sB := newMemDurableStore(), newMemDurableStore()
	tA, tB := newMemDurableStore(), newMemDurableStore()
	mods := []Module{
		methModule{id: "a", methods: []string{"/a.v1.S/M"}},
		methModule{id: "b", methods: []string{"/b.v1.S/M"}},
	}
	regs := []durableIdemRegistration{
		{moduleID: "a", store: sA, tx: tA},
		{moduleID: "b", store: sB, tx: tB},
	}
	h.build(mods, regs)

	if h.storeFor("/a.v1.S/M") != middleware.DurableIdempotencyStore(sA) {
		t.Error("method /a.v1.S/M must route to module a's store")
	}
	if h.storeFor("/b.v1.S/M") != middleware.DurableIdempotencyStore(sB) {
		t.Error("method /b.v1.S/M must route to module b's store")
	}
	if h.txForMethod("/a.v1.S/M") != persistence.TxRunner(tA) {
		t.Error("method /a.v1.S/M must route to module a's tx")
	}
	// An unowned method with >1 registration (single==nil) routes to the fallback.
	if h.storeFor("/unknown/M") != middleware.DurableIdempotencyStore(h.fallback) {
		t.Error("an unowned method must route to the in-memory fallback")
	}
}

func TestHostDurableDedup_OwnedButUnregistered_RoutesToFallback(t *testing.T) {
	// Two modules exist; only "a" registers a store. A request to module "b"'s method
	// (owned by b, which did NOT register) must route to the ISOLATED fallback, NEVER
	// to module a's store/tx — otherwise, under dedicated-DB isolation, b's effect would
	// bind to a's backend.
	h := newHostDurableDedup()
	sA, tA := newMemDurableStore(), newMemDurableStore()
	h.build(
		[]Module{
			methModule{id: "a", methods: []string{"/a.v1.S/M"}},
			methModule{id: "b", methods: []string{"/b.v1.S/M"}},
		},
		[]durableIdemRegistration{{moduleID: "a", store: sA, tx: tA}},
	)
	if got := h.storeFor("/b.v1.S/M"); got != middleware.DurableIdempotencyStore(h.fallback) {
		t.Errorf("an owned-but-unregistered module's method must route to the fallback, not another module's store")
	}
	if got := h.txForMethod("/b.v1.S/M"); got != persistence.TxRunner(h.fallback) {
		t.Errorf("an owned-but-unregistered module's method tx must route to the fallback")
	}
	// Sanity: module a's own method still routes to a's store.
	if h.storeFor("/a.v1.S/M") != middleware.DurableIdempotencyStore(sA) {
		t.Errorf("module a's method must route to its own store")
	}
	// And unregisteredModules names b (for the boot warning).
	miss := h.unregisteredModules([]Module{
		methModule{id: "a", methods: []string{"/a.v1.S/M"}},
		methModule{id: "b", methods: []string{"/b.v1.S/M"}},
	})
	if len(miss) != 1 || miss[0] != "b" {
		t.Errorf("unregisteredModules must name b, got %v", miss)
	}
}

func TestHostDurableDedup_SingleRegistrationFallback(t *testing.T) {
	h := newHostDurableDedup()
	sA, tA := newMemDurableStore(), newMemDurableStore()
	h.build([]Module{methModule{id: "a", methods: []string{"/a.v1.S/M"}}},
		[]durableIdemRegistration{{moduleID: "a", store: sA, tx: tA}})
	// With exactly one registration, even an unowned method routes to that store/tx.
	if h.storeFor("/unmapped/X") != middleware.DurableIdempotencyStore(sA) {
		t.Error("single registration must serve any method")
	}
	if h.txForMethod("/unmapped/X") != persistence.TxRunner(tA) {
		t.Error("single registration tx must serve any method")
	}
}

func TestHostDurableDedup_GCAggregates(t *testing.T) {
	h := newHostDurableDedup()
	sA := newMemDurableStore()
	h.build([]Module{methModule{id: "a", methods: []string{"/a/M"}}, methModule{id: "b", methods: []string{"/b/M"}}},
		[]durableIdemRegistration{{moduleID: "a", store: sA, tx: sA}})
	past := time.Now().Add(-time.Hour)
	// Seed one expired record in the registered store and one in the fallback.
	seedExpired(t, sA, persistence.IdempotencyKey{Method: "/a/M", RequestID: "r1"}, past)
	seedExpired(t, h.fallback, persistence.IdempotencyKey{Method: "/b/M", RequestID: "r2"}, past)
	n, err := h.GC(context.Background(), time.Now())
	if err != nil || n != 2 {
		t.Fatalf("GC must sweep registered stores + fallback, got n=%d err=%v", n, err)
	}
}

func TestHostDurableDedup_VerifyMigrated_FailLoud(t *testing.T) {
	h := newHostDurableDedup()
	h.build([]Module{methModule{id: "a", methods: []string{"/a/M"}}},
		[]durableIdemRegistration{{moduleID: "a", store: errLookupStore{newMemDurableStore()}, tx: newMemDurableStore()}})
	err := h.verifyMigrated(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not migrated") || !strings.Contains(err.Error(), "RequestIdempotencyMigrationModels") {
		t.Fatalf("verifyMigrated must fail loud naming the migration models, got %v", err)
	}
}

func TestHostDurableDedup_ExactlyOnce_ThroughInterceptor(t *testing.T) {
	h := newHostDurableDedup()
	sA := newMemDurableStore()
	h.build([]Module{methModule{id: "a", methods: []string{"/a.v1.S/Create"}}},
		[]durableIdemRegistration{{moduleID: "a", store: sA, tx: sA}})
	intc := middleware.DurableDeduplicateUnary(middleware.DurableDedup{Store: h, Tx: h})
	info := &grpc.UnaryServerInfo{FullMethod: "/a.v1.S/Create"}
	calls := 0
	handler := func(context.Context, any) (any, error) { calls++; return wrapperspb.String("gen"), nil }

	if _, err := intc(context.Background(), req("r1", "b"), info, handler); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if _, err := intc(context.Background(), req("r1", "b"), info, handler); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("routed durable path must be exactly-once, got %d calls", calls)
	}
	// The record landed in the routed module store, not the fallback.
	if _, ok, _ := sA.Lookup(context.Background(), persistence.IdempotencyKey{Method: info.FullMethod, RequestID: "r1"}); !ok {
		t.Fatal("the completed record must be in the routed module store")
	}
}

// --- GC loop lifecycle --------------------------------------------------------

func TestRunIdempotencyGC_SweepsAndStops(t *testing.T) {
	g := &gcCounter{memDurableStore: newMemDurableStore()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go runIdempotencyGC(ctx, g, 5*time.Millisecond, slog.Default(), done)

	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GC goroutine did not stop after ctx cancel")
	}
	if atomic.LoadInt32(&g.n) < 1 {
		t.Fatalf("GC must have swept at least once, got %d", g.n)
	}
	before := atomic.LoadInt32(&g.n)
	time.Sleep(30 * time.Millisecond)
	if atomic.LoadInt32(&g.n) != before {
		t.Fatal("GC must not run after the goroutine stopped")
	}
}

// seedExpired commits an already-expired completed record into an in-memory store.
func seedExpired(t *testing.T, s *memDurableStore, key persistence.IdempotencyKey, expiry time.Time) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[memKey(key)] = memDurableRecord{
		rec:       persistence.IdempotencyRecord{Status: persistence.StatusCompleted},
		expiresAt: expiry,
	}
}
