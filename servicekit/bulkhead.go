package servicekit

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// FailurePolicy is a module's failure posture in a composed host (proposal §5.9). A
// single process loses OS-level isolation between modules, so the host provides
// in-process bulkheads; FailurePolicy decides what a module failure does to the host.
type FailurePolicy string

const (
	// FailurePolicyUnset defers to the host default. The default is FailHost for a
	// single-module (standalone) host — a service whose only module fails should fail
	// fast — and is configurable per composition otherwise.
	FailurePolicyUnset FailurePolicy = ""
	// FailHost marks a CORE module: a failure (a background-job/dispatcher crash, a
	// panic, a fatal readiness state) takes the whole host down fail-fast. Use it for
	// a module the suite cannot meaningfully run without.
	FailHost FailurePolicy = "fail-host"
	// Degraded marks an OPTIONAL module: a failure is ISOLATED to that module — it is
	// marked unready (so /readyz reflects the degradation) but the host stays up and
	// the other modules keep serving. Use it for a module whose absence is tolerable.
	Degraded FailurePolicy = "degraded"
)

// supervisor runs each module's background work (event relays, consumers, background
// jobs) in its OWN goroutine keyed by module ID, with a PANIC BOUNDARY around the
// work so a panic in one module is recovered, attributed to that module, and routed
// through its [FailurePolicy] — it does NOT crash another module or the host (unless
// the policy is FailHost). It is the in-process bulkhead the proposal calls for, built
// from the stdlib (no new dependency).
//
// Health attribution: a degraded module marks ITSELF unready via a per-module
// [moduleReadiness] gate (registered on the shared server's readiness aggregator), so
// the host's /readyz reflects exactly which module is degraded without sinking the
// whole host. A FailHost module instead cancels the host context (fail-fast).
type supervisor struct {
	logger   *slog.Logger
	policyOf func(moduleID string) FailurePolicy // resolved per-module policy
	failHost context.CancelCauseFunc             // cancels the host context (fail-fast)

	mu    sync.Mutex
	readi map[string]*moduleReadiness // per-module readiness gate
	wg    sync.WaitGroup
}

// newSupervisor builds a supervisor. failHost cancels the host's root context with a
// cause (fail-fast for a FailHost module); policyOf resolves a module's FailurePolicy.
func newSupervisor(logger *slog.Logger, failHost context.CancelCauseFunc, policyOf func(string) FailurePolicy) *supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &supervisor{
		logger:   logger,
		policyOf: policyOf,
		failHost: failHost,
		readi:    map[string]*moduleReadiness{},
	}
}

// readinessFor returns (creating on first use) the module's readiness gate. The host
// registers it on the shared server so /readyz aggregates it; the supervisor flips it
// to unready when a degraded module fails.
func (s *supervisor) readinessFor(moduleID string) *moduleReadiness {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.readi[moduleID]
	if !ok {
		r = &moduleReadiness{name: moduleID + ".module"}
		r.ready.Store(true)
		s.readi[moduleID] = r
	}
	return r
}

// Go runs fn (a module's relay/consumer/job loop) in a supervised goroutine with a
// panic boundary. role labels the work (relay/consumer/job) in logs + errors. A panic
// is recovered, attributed to moduleID, and routed through the module's failure
// policy: Degraded marks the module unready and keeps the host up; FailHost cancels
// the host context (fail-fast).
func (s *supervisor) Go(moduleID, role string, fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.recoverModule(moduleID, role)
		fn()
	}()
}

// recoverModule is the per-module panic boundary. It recovers a panic in a module's
// background work, attributes it to the module, records it, and applies the module's
// failure policy. It is deferred at the top of each supervised goroutine so the panic
// never unwinds past the module boundary.
func (s *supervisor) recoverModule(moduleID, role string) {
	r := recover()
	if r == nil {
		return
	}
	err := fmt.Errorf("module %q %s panicked: %v", moduleID, role, r)
	s.logger.Error("servicekit: contained module panic",
		"module", moduleID, "role", role, "panic", r, "stack", string(debug.Stack()))
	s.fail(moduleID, err)
}

// reportError routes a NON-panic module error (a relay/consumer/job returned an error)
// through the same failure-policy machinery. It is called by the dispatchers' onErr
// callbacks.
func (s *supervisor) reportError(moduleID string, err error) {
	if err == nil {
		return
	}
	s.logger.Error("servicekit: module error", "module", moduleID, "err", err)
	s.fail(moduleID, err)
}

// fail applies the module's FailurePolicy to a failure: either mark the module unready
// (Degraded — host stays up) or cancel the host context (FailHost — fail-fast).
func (s *supervisor) fail(moduleID string, err error) {
	policy := FailHost
	if s.policyOf != nil {
		if p := s.policyOf(moduleID); p != FailurePolicyUnset {
			policy = p
		}
	}
	switch policy {
	case Degraded:
		// Isolate: mark the module unready (so /readyz reflects it) but keep the host
		// and the other modules up.
		s.readinessFor(moduleID).setUnready(err)
	default: // FailHost (and the conservative default)
		// Fail fast: take the whole host down with the attributed cause.
		if s.failHost != nil {
			s.failHost(fmt.Errorf("servicekit: core module %q failed: %w", moduleID, err))
		}
	}
}

// wait blocks until every supervised goroutine has returned (host shutdown).
func (s *supervisor) wait() { s.wg.Wait() }

// guardCall runs a synchronous in-line call (e.g. a module's Register, or one
// dispatch) inside the module's panic boundary, converting a panic into an error
// attributed to the module rather than letting it unwind the host. It returns the
// recovered panic as an error so the caller can route it through the failure policy.
func guardCall(moduleID, what string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("module %q %s panicked: %v\n%s", moduleID, what, r, debug.Stack())
		}
	}()
	return fn()
}

// moduleReadiness is a per-module readiness gate registered on the shared server's
// readiness aggregator. It is READY until the supervisor marks it unready (a degraded
// module's isolated failure), at which point /readyz fails with the module's cause —
// attributing the degradation to exactly that module without sinking the host.
type moduleReadiness struct {
	name  string
	ready atomic.Bool
	mu    sync.Mutex
	cause error
}

// Name implements health.Check.
func (m *moduleReadiness) Name() string { return m.name }

// Check implements health.Check: nil while ready, the module's failure cause once the
// supervisor marks it unready.
func (m *moduleReadiness) Check(_ context.Context) error {
	if m.ready.Load() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cause != nil {
		return fmt.Errorf("module degraded: %w", m.cause)
	}
	return fmt.Errorf("module degraded")
}

// setUnready flips the gate to unready and records the cause.
func (m *moduleReadiness) setUnready(cause error) {
	m.mu.Lock()
	m.cause = cause
	m.mu.Unlock()
	m.ready.Store(false)
}
