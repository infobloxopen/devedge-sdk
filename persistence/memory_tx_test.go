package persistence

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// parent and child model an aggregate-shaped pair: a child carries its parent's
// key, mirroring the single parent+child atomic write F030 (AC-1/AC-2) targets.
type parent struct {
	ID    string
	State string
}

type child struct {
	ID       string
	ParentID string
}

// TestMemoryAtomically_RollbackOnError is AC-1 for the in-memory backend: a
// handler loads a parent, checks its state, writes a child — all through the
// neutral seam inside Atomically — and a forced error mid-fn rolls back with no
// partial write. Parent and child are SEPARATE repositories joined by one runner.
func TestMemoryAtomically_RollbackOnError(t *testing.T) {
	ctx := context.Background()
	parents := NewMemoryRepository(func(p parent) string { return p.ID })
	children := NewMemoryRepository(func(c child) string { return c.ID })
	tx := NewMemoryTxRunner(parents, children)

	if _, err := parents.Create(ctx, parent{ID: "p1", State: "OPEN"}); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("forced mid-fn failure")
	err := tx.Atomically(ctx, func(txCtx context.Context) error {
		// load → check → write, spanning both repositories.
		p, gerr := parents.Get(txCtx, "p1")
		if gerr != nil {
			return gerr
		}
		if p.State != "OPEN" {
			return errors.New("parent not open")
		}
		if _, cerr := children.Create(txCtx, child{ID: "c1", ParentID: "p1"}); cerr != nil {
			return cerr
		}
		if _, uerr := parents.Update(txCtx, "p1", parent{ID: "p1", State: "SHIPPED"}); uerr != nil {
			return uerr
		}
		return wantErr // forced failure after both writes
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Atomically: want forced error, got %v", err)
	}

	// No partial write: the child was never persisted...
	if _, gerr := children.Get(ctx, "c1"); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("child c1 should not exist after rollback, got %v", gerr)
	}
	// ...and the parent state was restored.
	p, gerr := parents.Get(ctx, "p1")
	if gerr != nil {
		t.Fatalf("parent p1 should still exist after rollback: %v", gerr)
	}
	if p.State != "OPEN" {
		t.Fatalf("parent state should be rolled back to OPEN, got %q", p.State)
	}
}

// TestMemoryAtomically_CommitOnSuccess is the happy path: the parent+child write
// is kept across both repositories when fn returns nil.
func TestMemoryAtomically_CommitOnSuccess(t *testing.T) {
	ctx := context.Background()
	parents := NewMemoryRepository(func(p parent) string { return p.ID })
	children := NewMemoryRepository(func(c child) string { return c.ID })
	tx := NewMemoryTxRunner(parents, children)

	if _, err := parents.Create(ctx, parent{ID: "p1", State: "OPEN"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Atomically(ctx, func(txCtx context.Context) error {
		if _, err := children.Create(txCtx, child{ID: "c1", ParentID: "p1"}); err != nil {
			return err
		}
		_, err := parents.Update(txCtx, "p1", parent{ID: "p1", State: "SHIPPED"})
		return err
	}); err != nil {
		t.Fatalf("Atomically: %v", err)
	}

	got, err := children.Get(ctx, "c1")
	if err != nil {
		t.Fatalf("child c1 should be committed: %v", err)
	}
	if got.ParentID != "p1" {
		t.Fatalf("committed child wrong: %+v", got)
	}
	p, err := parents.Get(ctx, "p1")
	if err != nil || p.State != "SHIPPED" {
		t.Fatalf("parent update should be committed, got %+v err=%v", p, err)
	}
}

// TestMemoryAtomically_Nested verifies nested Atomically joins the outer
// transaction (no-op begin) and rolls the whole thing back as one unit.
func TestMemoryAtomically_Nested(t *testing.T) {
	ctx := context.Background()
	parents := NewMemoryRepository(func(p parent) string { return p.ID })
	children := NewMemoryRepository(func(c child) string { return c.ID })
	tx := NewMemoryTxRunner(parents, children)

	boom := errors.New("rollback")
	err := tx.Atomically(ctx, func(txCtx context.Context) error {
		if _, err := parents.Create(txCtx, parent{ID: "p1", State: "OPEN"}); err != nil {
			return err
		}
		// Nested call on the same runner joins the outer tx — no new lock/snapshot.
		return tx.Atomically(txCtx, func(txCtx2 context.Context) error {
			if _, err := children.Create(txCtx2, child{ID: "c1", ParentID: "p1"}); err != nil {
				return err
			}
			return boom
		})
	})
	if !errors.Is(err, boom) {
		t.Fatalf("nested Atomically: want rollback error, got %v", err)
	}
	if _, err := parents.Get(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outer write must roll back when the nested fn fails, got %v", err)
	}
	if _, err := children.Get(ctx, "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nested write must roll back, got %v", err)
	}
}

// TestMemoryAtomically_RollbackOnPanic verifies a panic inside fn also discards
// the work (and re-panics so the caller is not silently fooled).
func TestMemoryAtomically_RollbackOnPanic(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(c child) string { return c.ID })
	tx := NewMemoryTxRunner(r)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = tx.Atomically(ctx, func(txCtx context.Context) error {
			if _, err := r.Create(txCtx, child{ID: "c1"}); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
	}()

	if _, err := r.Get(ctx, "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("child c1 should be discarded after panic, got %v", err)
	}
}

// TestMemoryAtomically_InvisibleUntilCommit is AC-2 for the in-memory backend. It
// proves BOTH halves of isolation, not merely that a lock blocks a reader:
//   - participation: a tx-bound read issued INSIDE Atomically sees the
//     transaction's own uncommitted write (the write joined the tx);
//   - isolation: a concurrent NON-tx reader does not observe that write until the
//     transaction commits (the write never escaped the tx mid-flight).
//
// The transaction holds the write lock for its duration, so the non-tx reader's
// Get blocks until the transaction completes and then observes the committed
// value — never a partial/in-flight state.
func TestMemoryAtomically_InvisibleUntilCommit(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(c child) string { return c.ID })
	tx := NewMemoryTxRunner(r)

	inTx := make(chan struct{})
	releaseTx := make(chan struct{})
	var (
		wg            sync.WaitGroup
		readerErr     error
		readerGotItem bool
		txReadErr     error
	)

	wg.Go(func() {
		_ = tx.Atomically(ctx, func(txCtx context.Context) error {
			if _, err := r.Create(txCtx, child{ID: "c1", ParentID: "p1"}); err != nil {
				return err
			}
			// Participation: the same tx-bound ctx must observe its own write.
			if _, err := r.Get(txCtx, "c1"); err != nil {
				txReadErr = err
			}
			close(inTx) // signal: write done, still inside the tx
			<-releaseTx // hold the tx open until the reader has tried to read
			return nil  // commit
		})
	})

	<-inTx
	// A read from another (non-tx) context: while the tx holds the write lock, the
	// reader blocks. We run it in a goroutine and assert it does not return before
	// commit.
	readDone := make(chan struct{})
	go func() {
		_, err := r.Get(ctx, "c1")
		readerErr = err
		readerGotItem = err == nil
		close(readDone)
	}()

	select {
	case <-readDone:
		// The reader returned before we released the tx. The only way Get returns
		// without blocking on the held write lock is if the lock were not held —
		// which would mean the write was visible mid-tx. Either way it is a failure.
		t.Fatal("concurrent reader was not blocked by the open transaction")
	case <-time.After(100 * time.Millisecond):
		// Expected: the reader is blocked on the write lock held by the open tx.
	}

	close(releaseTx) // let the tx commit and release the lock
	<-readDone       // the reader now unblocks
	wg.Wait()

	if txReadErr != nil {
		t.Fatalf("tx-bound read inside Atomically must see its own uncommitted write, got %v", txReadErr)
	}
	if readerErr != nil {
		t.Fatalf("reader after commit: want the committed child, got %v", readerErr)
	}
	if !readerGotItem {
		t.Fatal("reader after commit should see the committed child")
	}
}

// TestMemoryAtomically_ConcurrentNoDeadlock stresses lock-ordering: many
// goroutines run Atomically through two different runners that enroll the SAME two
// repositories in OPPOSITE declaration orders. Without a stable global lock order
// this is the classic A-then-B vs B-then-A deadlock; the runner sorts participants
// by a stable per-repo id so both always lock in the same order. Run under -race,
// this also asserts the tx-bound writes (which skip per-op locking) never race a
// concurrent non-tx reader. The test simply must finish.
func TestMemoryAtomically_ConcurrentNoDeadlock(t *testing.T) {
	ctx := context.Background()
	a := NewMemoryRepository(func(p parent) string { return p.ID })
	b := NewMemoryRepository(func(c child) string { return c.ID })

	// Two runners over the same repos, declared in opposite order. After sorting by
	// the stable id both acquire locks in the same order — no deadlock.
	txAB := NewMemoryTxRunner(a, b)
	txBA := NewMemoryTxRunner(b, a)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		runner := txAB
		if i%2 == 1 {
			runner = txBA
		}
		wg.Go(func() {
			_ = runner.Atomically(ctx, func(txCtx context.Context) error {
				_, _ = a.Create(txCtx, parent{ID: "p", State: "X"})
				_, _ = b.Create(txCtx, child{ID: "c", ParentID: "p"})
				return errors.New("always roll back") // keep state empty for the next iteration
			})
		})
		// A concurrent non-tx reader on the same repos, to exercise the lock gate.
		wg.Go(func() {
			_, _ = a.Get(ctx, "p")
			_, _, _ = b.List(ctx, ListOptions{})
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Atomically deadlocked (lock-ordering not stable)")
	}
}

// TestMemoryAtomically_DuplicateRepoNoSelfDeadlock guards the footgun of passing
// the same repository twice to one runner: without de-duplication Atomically would
// lock its non-reentrant write lock twice and self-deadlock.
func TestMemoryAtomically_DuplicateRepoNoSelfDeadlock(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(c child) string { return c.ID })
	tx := NewMemoryTxRunner(r, r, r) // same repo three times

	done := make(chan error, 1)
	go func() {
		done <- tx.Atomically(ctx, func(txCtx context.Context) error {
			_, err := r.Create(txCtx, child{ID: "c1"})
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Atomically with a duplicate repo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Atomically self-deadlocked on a duplicate repository")
	}
	if _, err := r.Get(ctx, "c1"); err != nil {
		t.Fatalf("committed write should be visible: %v", err)
	}
}

// TestMemoryAtomically_DiscardedOnRollbackVisibleNothing is the rollback half of
// AC-2: after a rolled-back transaction, a reader sees nothing of the write.
func TestMemoryAtomically_DiscardedOnRollbackVisibleNothing(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository(func(c child) string { return c.ID })
	tx := NewMemoryTxRunner(r)

	boom := errors.New("rollback")
	if err := tx.Atomically(ctx, func(txCtx context.Context) error {
		if _, err := r.Create(txCtx, child{ID: "c1"}); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Atomically: want rollback error, got %v", err)
	}

	if _, err := r.Get(ctx, "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after rollback the write must be invisible, got %v", err)
	}
}
