package servicekit_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/servicekit"
)

// extStore is a minimal durable idempotency store + tx runner defined in the external
// test so a test module can register one WITHOUT importing gormtx (a nested module). It
// counts GC sweeps and can force Lookup to fail (the un-migrated boot-probe case).
type extStore struct {
	gcCalls   int32
	lookupErr error
}

func (s *extStore) Lookup(context.Context, persistence.IdempotencyKey) (persistence.IdempotencyRecord, bool, error) {
	if s.lookupErr != nil {
		return persistence.IdempotencyRecord{}, false, s.lookupErr
	}
	return persistence.IdempotencyRecord{}, false, nil
}
func (s *extStore) Claim(context.Context, persistence.IdempotencyKey, string, time.Duration) (persistence.IdempotencyRecord, bool, error) {
	return persistence.IdempotencyRecord{}, true, nil
}
func (s *extStore) Complete(context.Context, persistence.IdempotencyKey, string, []byte) error {
	return nil
}
func (s *extStore) Abandon(context.Context, persistence.IdempotencyKey) (bool, error) {
	return false, nil
}
func (s *extStore) GC(context.Context, time.Time) (int64, error) {
	atomic.AddInt32(&s.gcCalls, 1)
	return 0, nil
}
func (s *extStore) Atomically(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// idemModule registers a gRPC method + rule and (optionally) enables durable
// idempotency with a supplied store, so a test can exercise App.EnableDurableIdempotency
// through servicekit.Run.
type idemModule struct {
	id     string
	method string
	reg    *servicekit.DurableIdempotencyRegistration // nil = do not enable
	enable bool                                        // call EnableDurableIdempotency even when reg is nil (nil-field test)
	regErr *error                                      // captures the returned error
	orphan bool                                        // record the method but contribute NO rule (fails the boot gate at Serve)
}

func (m idemModule) Descriptor() servicekit.Descriptor {
	d := servicekit.Descriptor{ID: m.id, Methods: []string{m.method}}
	if !m.orphan {
		d.AuthzRules = []authz.MethodRule{{Method: m.method, Public: true}}
	}
	return d
}

func (m idemModule) Register(_ context.Context, app *servicekit.App) error {
	app.Server.RecordMethods(m.method)
	if !m.orphan {
		app.Server.AddRules(authz.MethodRule{Method: m.method, Public: true})
	}
	if m.reg != nil {
		if err := app.EnableDurableIdempotency(*m.reg); err != nil {
			if m.regErr != nil {
				*m.regErr = err
			}
			return err
		}
	} else if m.enable {
		if err := app.EnableDurableIdempotency(servicekit.DurableIdempotencyRegistration{}); err != nil {
			if m.regErr != nil {
				*m.regErr = err
			}
			return err
		}
	}
	return nil
}

func TestEnableDurableIdempotency_WithoutOptIn_FailsLoud(t *testing.T) {
	st := &extStore{}
	m := idemModule{
		id:     "svc",
		method: "/svc.v1.S/Create",
		reg:    &servicekit.DurableIdempotencyRegistration{Store: st, Tx: st},
	}
	// HostConfig.DurableIdempotency is NOT set → EnableDurableIdempotency must fail.
	err := servicekit.Run(servicekit.HostConfig{Modules: []servicekit.Module{m}, GRPCAddr: ":0", Context: cancelledCtx()})
	if err == nil || !strings.Contains(err.Error(), "HostConfig.DurableIdempotency is not set") {
		t.Fatalf("enabling durable idempotency without the host opt-in must fail loud, got %v", err)
	}
}

func TestEnableDurableIdempotency_NilStore_FailsLoud(t *testing.T) {
	m := idemModule{id: "svc", method: "/svc.v1.S/Create", enable: true} // reg fields nil
	err := servicekit.Run(servicekit.HostConfig{
		Modules:            []servicekit.Module{m},
		GRPCAddr:           ":0",
		Context:            cancelledCtx(),
		DurableIdempotency: &servicekit.DurableIdempotencyConfig{},
	})
	if err == nil || !strings.Contains(err.Error(), "Store and Tx are both required") {
		t.Fatalf("a nil Store/Tx must fail loud, got %v", err)
	}
}

func TestRun_DurableIdempotency_BootProbeFailsLoud(t *testing.T) {
	st := &extStore{lookupErr: errors.New("no such table: idempotency_keys")}
	m := idemModule{
		id:     "svc",
		method: "/svc.v1.S/Create",
		reg:    &servicekit.DurableIdempotencyRegistration{Store: st, Tx: st},
	}
	err := servicekit.Run(servicekit.HostConfig{
		Modules:            []servicekit.Module{m},
		GRPCAddr:           ":0",
		Context:            cancelledCtx(),
		DurableIdempotency: &servicekit.DurableIdempotencyConfig{},
	})
	if err == nil || !strings.Contains(err.Error(), "not migrated") {
		t.Fatalf("an un-migrated idempotency table must fail loud at boot, got %v", err)
	}
}

func TestRun_DurableIdempotency_BootsAndGCRuns(t *testing.T) {
	st := &extStore{}
	m := idemModule{
		id:     "svc",
		method: "/svc.v1.S/Create",
		reg:    &servicekit.DurableIdempotencyRegistration{Store: st, Tx: st},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- servicekit.Run(servicekit.HostConfig{
			Modules:  []servicekit.Module{m},
			GRPCAddr: ":0",
			Context:  ctx,
			DurableIdempotency: &servicekit.DurableIdempotencyConfig{
				GCInterval: 10 * time.Millisecond,
			},
		})
	}()
	// Let the host boot + the GC sweep tick a few times.
	time.Sleep(120 * time.Millisecond)
	if atomic.LoadInt32(&st.gcCalls) < 1 {
		t.Fatalf("host-scheduled GC must have swept at least once, got %d", st.gcCalls)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown expected, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel (leaked GC goroutine?)")
	}
}

// TestRun_DurableIdempotency_ServeErrorDoesNotDeadlock is the regression for the
// shutdown-deadlock bug: with durable idempotency + GC enabled and a LIVE (never
// cancelled) context, a Serve-time error (here, an orphan method failing the boot gate)
// must return promptly. Before the fix, the cleanup defer waited on the GC goroutine,
// which only exits on ctx cancel — so Run hung forever.
func TestRun_DurableIdempotency_ServeErrorDoesNotDeadlock(t *testing.T) {
	st := &extStore{}
	m := idemModule{
		id:     "svc",
		method: "/svc.v1.S/Orphan",
		orphan: true, // no rule → server boot gate fails at Serve
		reg:    &servicekit.DurableIdempotencyRegistration{Store: st, Tx: st},
	}
	done := make(chan error, 1)
	go func() {
		done <- servicekit.Run(servicekit.HostConfig{
			Modules:  []servicekit.Module{m},
			GRPCAddr: ":0",
			Context:  context.Background(), // LIVE — only the deadlock fix can unblock shutdown
			DurableIdempotency: &servicekit.DurableIdempotencyConfig{
				GCInterval: 5 * time.Millisecond,
			},
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "undeclared") {
			t.Fatalf("expected the boot-gate 'undeclared' error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run deadlocked: Serve returned an error but the GC-wait never unblocked")
	}
}

func TestRun_DurableIdempotency_DisableGC(t *testing.T) {
	st := &extStore{}
	m := idemModule{
		id:     "svc",
		method: "/svc.v1.S/Create",
		reg:    &servicekit.DurableIdempotencyRegistration{Store: st, Tx: st},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- servicekit.Run(servicekit.HostConfig{
			Modules:            []servicekit.Module{m},
			GRPCAddr:           ":0",
			Context:            ctx,
			DurableIdempotency: &servicekit.DurableIdempotencyConfig{DisableGC: true, GCInterval: 5 * time.Millisecond},
		})
	}()
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&st.gcCalls); got != 0 {
		t.Fatalf("DisableGC must stop the sweep, got %d calls", got)
	}
	cancel()
	<-done
}
