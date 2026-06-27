package server

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// countReadinessLoops reports how many goroutines are currently parked in
// (*Server).runReadinessLoop, read from a full stack dump.
func countReadinessLoops() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "runReadinessLoop")
}

// TestServe_ReadinessLoop_StopsOnErrorReturn is the regression guard for the
// readiness-loop goroutine-leak fix. It exercises the path that previously
// leaked: Serve returns via its INTERNAL error channel (a fatal runtime serve
// failure) while the caller's context is NEVER cancelled. Before the fix the
// loop was bound only to the caller's context, so it kept running — driving the
// already-stopped gRPC health server — forever. After the fix Serve owns the
// loop via a derived context cancelled on every return path, so the goroutine
// stops when Serve returns regardless of the caller's context.
//
// To force the error return deterministically AFTER the loop has started, we let
// Serve bind + start the loop, then close the gRPC listener out from under it so
// grpcSrv.Serve returns a fatal accept error onto errCh.
func TestServe_ReadinessLoop_StopsOnErrorReturn(t *testing.T) {
	base := countReadinessLoops()

	s, err := New(Config{GRPCAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A context the test NEVER cancels — the leak path depended on this.
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background()) }()

	bound := func() bool {
		s.lisMu.Lock()
		defer s.lisMu.Unlock()
		return s.grpcLis != nil
	}

	// Wait until the loop is running and the listener is bound.
	deadline := time.Now().Add(3 * time.Second)
	for (countReadinessLoops() <= base || !bound()) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if countReadinessLoops() <= base {
		t.Fatal("readiness loop never started")
	}

	// Close the gRPC listener to make grpcSrv.Serve fail fatally -> errCh -> Serve
	// returns with the caller's context still live.
	s.lisMu.Lock()
	lis := s.grpcLis
	s.lisMu.Unlock()
	_ = lis.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s after the listener failed")
	}

	// The loop must be gone now that Serve has returned (brief scheduler window).
	deadline = time.Now().Add(2 * time.Second)
	for countReadinessLoops() > base && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := countReadinessLoops(); got > base {
		t.Fatalf("readiness loop leaked after Serve returned: %d runReadinessLoop goroutines remain (baseline %d)", got, base)
	}
}
