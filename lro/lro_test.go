package lro_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/lro"
)

// --- MemoryStore ---

func TestMemoryStore_CRUD(t *testing.T) {
	s := lro.NewMemoryStore(time.Hour)
	ctx := context.Background()

	op := &lro.Operation{Name: lro.OperationName("abc"), CreateTime: time.Now(), UpdateTime: time.Now()}
	if err := s.Create(ctx, op); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(ctx, op.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != op.Name {
		t.Errorf("Get name = %q, want %q", got.Name, op.Name)
	}

	// Update to done.
	op.Done = true
	op.Response = "hello"
	if err := s.Update(ctx, op); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got2, _ := s.Get(ctx, op.Name)
	if !got2.Done {
		t.Error("after Update: want Done=true")
	}

	// Delete.
	if err := s.Delete(ctx, op.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, op.Name); !errors.Is(err, lro.ErrNotFound) {
		t.Errorf("after Delete: want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_GetNotFound(t *testing.T) {
	s := lro.NewMemoryStore(time.Hour)
	_, err := s.Get(context.Background(), "operations/missing")
	if !errors.Is(err, lro.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_TTLExpiresCompleted(t *testing.T) {
	// Completed ops expire after a very short TTL; pending ones do not.
	s := lro.NewMemoryStore(50 * time.Millisecond)
	ctx := context.Background()

	op := &lro.Operation{Name: lro.OperationName("ttl"), CreateTime: time.Now(), UpdateTime: time.Now()}
	_ = s.Create(ctx, op)

	op.Done = true
	_ = s.Update(ctx, op)

	time.Sleep(100 * time.Millisecond)

	_, err := s.Get(ctx, op.Name)
	if !errors.Is(err, lro.ErrNotFound) {
		t.Errorf("after TTL: want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_PendingDoesNotExpire(t *testing.T) {
	// Pending (not done) operations must not expire even with a short TTL.
	s := lro.NewMemoryStore(10 * time.Millisecond)
	ctx := context.Background()

	op := &lro.Operation{Name: lro.OperationName("pending"), CreateTime: time.Now(), UpdateTime: time.Now()}
	_ = s.Create(ctx, op)

	time.Sleep(30 * time.Millisecond)

	_, err := s.Get(ctx, op.Name)
	if err != nil {
		t.Errorf("pending op should not expire: %v", err)
	}
}

func TestMemoryStore_UpdateIdempotentOnDone(t *testing.T) {
	s := lro.NewMemoryStore(time.Hour)
	ctx := context.Background()

	op := &lro.Operation{Name: lro.OperationName("idem"), Done: true, Response: "first", CreateTime: time.Now(), UpdateTime: time.Now()}
	_ = s.Create(ctx, op)

	// Second update must be a no-op because op.Done is already true in the store.
	op2 := *op
	op2.Response = "second"
	if err := s.Update(ctx, &op2); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get(ctx, op.Name)
	if got.Response != "first" {
		t.Errorf("double-complete: response should stay %q, got %q", "first", got.Response)
	}
}

func TestMemoryStore_List(t *testing.T) {
	s := lro.NewMemoryStore(time.Hour)
	ctx := context.Background()

	for _, name := range []string{"a", "b", "c"} {
		_ = s.Create(ctx, &lro.Operation{Name: lro.OperationName(name)})
	}
	ops, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ops) != 3 {
		t.Errorf("List: want 3, got %d", len(ops))
	}
}

// --- Manager ---

func TestManager_SubmitAndComplete(t *testing.T) {
	store := lro.NewMemoryStore(time.Hour)
	mgr := lro.NewManager(store)
	ctx := context.Background()

	op, err := mgr.Submit(ctx, "meta", func(ctx context.Context) (any, error) {
		return "done-result", nil
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if op.Done {
		t.Error("Submit should return pending op (Done=false)")
	}

	// Poll until done.
	final, err := lro.WaitOperation(ctx, store, op.Name, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitOperation: %v", err)
	}
	if !final.Done {
		t.Error("final op should be Done=true")
	}
	if final.Response != "done-result" {
		t.Errorf("response = %v, want %q", final.Response, "done-result")
	}
	if final.Err != nil {
		t.Errorf("unexpected error: %v", final.Err)
	}
}

func TestManager_SubmitFnError(t *testing.T) {
	store := lro.NewMemoryStore(time.Hour)
	mgr := lro.NewManager(store)
	ctx := context.Background()

	sentinel := errors.New("fn failed")
	op, err := mgr.Submit(ctx, nil, func(ctx context.Context) (any, error) {
		return nil, sentinel
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	final, err := lro.WaitOperation(ctx, store, op.Name, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitOperation: %v", err)
	}
	if !errors.Is(final.Err, sentinel) {
		t.Errorf("Err = %v, want %v", final.Err, sentinel)
	}
}

func TestManager_CtxCancelledBeforeSubmitReturnsError(t *testing.T) {
	// Submitting with an already-cancelled ctx must return an error immediately
	// and not create any operation in the store.
	store := lro.NewMemoryStore(time.Hour)
	mgr := lro.NewManager(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Submit

	_, err := mgr.Submit(ctx, nil, func(ctx context.Context) (any, error) {
		return "ok", nil
	})
	if err == nil {
		t.Error("Submit with cancelled ctx: want error, got nil")
	}
}

// --- WaitOperation ---

func TestWaitOperation_Timeout(t *testing.T) {
	store := lro.NewMemoryStore(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	op := &lro.Operation{Name: lro.OperationName("wait"), CreateTime: time.Now(), UpdateTime: time.Now()}
	_ = store.Create(context.Background(), op)

	_, err := lro.WaitOperation(ctx, store, op.Name, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want DeadlineExceeded, got %v", err)
	}
}

func TestOperationName(t *testing.T) {
	if got := lro.OperationName("123"); got != "operations/123" {
		t.Errorf("got %q, want %q", got, "operations/123")
	}
}
