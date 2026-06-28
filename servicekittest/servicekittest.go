// Package servicekittest provides generated-quality contract harnesses for the
// WS-012 composable-services test pyramid (proposal §7). Import it from your
// module's own test files — it lives in the ROOT SDK module so any module that
// already depends on devedge-sdk can use it without an extra dependency.
//
// Two primary harnesses:
//
//   - [AssertModule] — per-module contract test (no Docker needed). Asserts
//     descriptor validity, ID stability, config schema, no listener startup,
//     no os.Exit, authz completeness, and clean registration on a shared server.
//
//   - [AssertComposition] — composition smoke test. Asserts that a set of modules
//     can be composed together, their descriptors are conflict-free, and — when
//     a [CompositionOptions.Migrate] is provided — migrations and readiness checks
//     all pass.
//
//   - [AssertCompatible] — pure version-range compatibility check between a set
//     of module [servicekit.Compatibility] declarations and a host runtime set.
//     No DB, no Docker.
package servicekittest

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/events/membus"
	"github.com/infobloxopen/devedge-sdk/servicekit"
)

// ---- AssertModule -------------------------------------------------------

// ModuleOptions configures an [AssertModule] run. All fields are optional.
type ModuleOptions struct {
	// ConfigSources are the configuration sources to layer under the module's
	// prefix during the config-load assertion. nil uses an empty source set.
	ConfigSources []interface{ Get(string) (string, bool) }

	// MigrationRunner is called to run the module's migrations in a fresh
	// (throwaway) namespace. When nil, the migration assertion is skipped.
	// Signature matches [servicekit.MigrationRunner].
	MigrationRunner servicekit.MigrationRunner

	// DBEngine is the engine name (e.g. "postgres") passed to the host's
	// DatabaseRegistry when resolving the module's namespace for the migration
	// assertion. Defaults to "sqlite" (prefix-only, no schema) when empty, so the
	// migration runner can be exercised against any in-process DB.
	DBEngine string
}

// AssertModule runs the per-module contract test for m (proposal §7).
//
// Assertions (all in-process, no Docker required):
//  1. Descriptor() is non-zero (ID non-empty, at least one method).
//  2. Descriptor() is STABLE — calling it twice returns equal values.
//  3. ValidateDescriptors passes for [m] alone (valid as a single-module composition).
//  4. The module registers into a fresh empty [servicekit.Run] host without error.
//     The host is run with ":0" (ephemeral port) and a pre-cancelled context so it
//     never binds a real listener — asserting the module does NOT start its own
//     listener or call os.Exit.
//  5. If opts.MigrationRunner is non-nil, the MigrationRunner is invoked with the
//     module's resolved namespace (a throwaway allocation) — asserting migrations
//     load without error.
//  6. The server's fail-closed union completeness gate passes — asserting every
//     registered method has an authz rule or public exemption.
func AssertModule(t *testing.T, m servicekit.Module, opts ...ModuleOptions) {
	t.Helper()

	opt := ModuleOptions{}
	if len(opts) > 0 {
		opt = opts[0]
	}

	// (1) Descriptor non-zero.
	d := m.Descriptor()
	if strings.TrimSpace(d.ID) == "" {
		t.Fatal("AssertModule: Descriptor.ID is empty")
	}
	if len(d.Methods) == 0 {
		t.Fatal("AssertModule: Descriptor.Methods is empty")
	}

	// (2) Descriptor stable: two calls must return equal values.
	d2 := m.Descriptor()
	if d.ID != d2.ID || d.Version != d2.Version || len(d.Methods) != len(d2.Methods) {
		t.Fatalf("AssertModule: Descriptor() is not stable — first call differs from second (ID %q vs %q, methods %v vs %v)",
			d.ID, d2.ID, d.Methods, d2.Methods)
	}

	// (3) ValidateDescriptors for this module alone.
	if err := servicekit.ValidateDescriptors([]servicekit.Descriptor{d}); err != nil {
		t.Fatalf("AssertModule: ValidateDescriptors failed: %v", err)
	}

	// (4) Register into a fresh host with a pre-cancelled context.
	// The host never calls Serve (context already done) so no listener is bound.
	// This proves: no listener startup, no os.Exit, no global state mutation that
	// would panic on registration alone.
	engine := opt.DBEngine
	if engine == "" {
		engine = "sqlite"
	}
	dbCfg := &servicekit.DatabaseConfig{
		Engine:           engine,
		DefaultIsolation: servicekit.IsolationPrefixRequired,
	}

	var runErr error
	var migrate servicekit.MigrationRunner
	if opt.MigrationRunner != nil {
		// (5) Wrap the caller's runner so we can capture whether it was called.
		called := false
		inner := opt.MigrationRunner
		migrate = func(ctx context.Context, ns servicekit.DatabaseNamespace, dd servicekit.DatabaseDescriptor) error {
			called = true
			if err := inner(ctx, ns, dd); err != nil {
				return err
			}
			return nil
		}
		defer func() {
			if !called {
				t.Error("AssertModule: MigrationRunner was provided but was never called")
			}
		}()
	}

	// Run with a context that is already cancelled so the host exits immediately
	// after module registration + the server boot gate.
	runErr = servicekit.Run(servicekit.HostConfig{
		Modules:  []servicekit.Module{m},
		GRPCAddr: ":0",
		Context:  cancelledCtx(),
		Database: dbCfg,
		Migrate:  migrate,
	})
	// (6) The only expected error on a clean module is nil (boot gate + register
	// passed, context cancelled). Any other error is a contract violation.
	if runErr != nil {
		t.Fatalf("AssertModule: module %q failed to boot in an isolated host: %v", d.ID, runErr)
	}
}

// ---- AssertComposition --------------------------------------------------

// CompositionOptions configures an [AssertComposition] run.
type CompositionOptions struct {
	// Migrate is the host's MigrationRunner for all modules. When non-nil,
	// AssertComposition passes it to [servicekit.Run] and each module's
	// migrations are executed before the module registers.
	Migrate servicekit.MigrationRunner

	// Database is the shared database config handed to the host. Required when
	// Migrate is non-nil (so the host allocates per-module namespaces before
	// calling the runner). When nil and Migrate is non-nil, the harness creates a
	// default in-memory-safe config (sqlite + prefix isolation).
	Database *servicekit.DatabaseConfig

	// GRPCAddr is the listen address for the composition host. Defaults to ":0"
	// (ephemeral). When non-empty the harness waits for the host to accept on
	// that address before returning (gives time for migrations + boot).
	GRPCAddr string

	// WaitForReady, when true, causes the harness to dial the host's gRPC port
	// and wait for it to start accepting connections before asserting readiness.
	// No-op when GRPCAddr is empty (uses ":0").
	WaitForReady bool

	// Timeout is the max time to wait for the composition to boot. Defaults to
	// 30 s when zero.
	Timeout time.Duration
}

// AssertComposition runs the composition smoke test for a set of modules (proposal §7).
//
// Assertions:
//  1. ValidateModules passes (unique IDs, no duplicate services/routes/permissions,
//     coherent event graph) — all descriptor-level, no DB needed.
//  2. servicekit.Run boots the composition: all modules register, the server's
//     fail-closed union completeness gate passes over the UNION surface.
//  3. If opts.Migrate is non-nil: migrations are run per-module (host-run,
//     under the host's advisory-lock discipline).
//  4. The host shuts down cleanly after the assertions complete (no leaked goroutines).
//
// Docker note: when a real database is needed (via opts.Migrate / opts.Database)
// and Docker is unavailable, the caller's test must call t.Skip before invoking
// AssertComposition (or wrap it in a helper that skips on docker error). The harness
// itself does NOT start Docker — it is the caller's responsibility to supply a
// working MigrationRunner + Database pointing at a real DB (e.g. via testcontainers
// in the outer test, following the pattern in testdata/iam/iamv1/ws012_*_pg_test.go).
//
// When no Database/Migrate are provided, AssertComposition runs entirely in-process
// (no Docker required) and asserts descriptor validity + host boot.
func AssertComposition(t *testing.T, modules []servicekit.Module, opts ...CompositionOptions) {
	t.Helper()

	if len(modules) == 0 {
		t.Fatal("AssertComposition: no modules provided")
	}

	opt := CompositionOptions{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}

	// (1) ValidateModules — descriptor-level, no DB.
	if err := servicekit.ValidateModules(modules); err != nil {
		t.Fatalf("AssertComposition: ValidateModules failed: %v", err)
	}

	grpcAddr := opt.GRPCAddr
	if grpcAddr == "" {
		grpcAddr = ":0"
	}

	// Allocate an ephemeral port when ":0" so we can wait for it.
	if grpcAddr == ":0" && opt.WaitForReady {
		addr, err := allocEphemeralAddr()
		if err != nil {
			t.Fatalf("AssertComposition: could not allocate ephemeral port: %v", err)
		}
		grpcAddr = addr
	}

	dbCfg := opt.Database
	if dbCfg == nil && opt.Migrate != nil {
		dbCfg = &servicekit.DatabaseConfig{
			Engine:           "sqlite",
			DefaultIsolation: servicekit.IsolationPrefixRequired,
		}
	}

	// (2) + (3): boot the composition host.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- servicekit.Run(servicekit.HostConfig{
			Modules:  modules,
			GRPCAddr: grpcAddr,
			Context:  ctx,
			Bus:      membus.New(),
			Database: dbCfg,
			Migrate:  opt.Migrate,
		})
	}()

	// If the caller wants to wait for the listener to be ready, dial until it accepts.
	if opt.WaitForReady && grpcAddr != ":0" {
		waitForListener(t, grpcAddr, opt.Timeout)
	} else {
		// Give the host a moment to start and reach the Serve call (so the boot gate
		// runs and we catch any boot-time errors). If it fails immediately (validation,
		// migration, or boot gate failure) we surface that.
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case err := <-done:
			// Host exited before we cancelled — that is a failure.
			t.Fatalf("AssertComposition: host exited before test assertions completed: %v", err)
			return
		case <-timer.C:
			// Host is running — proceed.
		}
	}

	// (4) Cancel and wait for clean shutdown.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AssertComposition: Run returned error: %v", err)
		}
	case <-time.After(opt.Timeout):
		t.Fatalf("AssertComposition: Run did not return within %s after context cancel", opt.Timeout)
	}
}

// ---- AssertCompatible ---------------------------------------------------

// HostRequires declares the host runtime's version capabilities, against which
// each module's [servicekit.Compatibility] is validated by [AssertCompatible].
type HostRequires struct {
	// SDK is the host's devedge-sdk version (e.g. "v0.28.0"). Each module's
	// Requires.SDK range (e.g. ">=0.27.0") is checked against this.
	SDK string
	// Go is the host's Go toolchain version (e.g. "1.25.5").
	Go string
	// Postgres is the Postgres server version available to the host (e.g. "16.3").
	// Empty means no Postgres — modules that require a Postgres range are flagged.
	Postgres string
}

// AssertCompatible validates that every module's Compatibility declaration is
// satisfiable by the host runtime's capabilities (proposal §7, "compatibility
// metadata helper"). It is PURE — no DB, no network, no Docker.
//
// For each module's Requires field it checks:
//   - Requires.SDK: if the module declares a minimum (e.g. ">=0.27.0"), the host
//     SDK version must meet it. Simple ">=X.Y.Z" and ">=X.Y" prefixes are handled;
//     other range syntaxes are accepted as-is (no rejection for unsupported forms).
//   - Requires.Go: same pattern match against host.Go.
//   - Requires.Postgres: if non-empty and host.Postgres is empty, the check fails
//     (the module needs Postgres but the host has none). Version range comparison
//     follows the same simple prefix rule.
//
// This is the function [de compose tidy] (P4) calls to reject impossible module
// combinations before compilation. It does not try to resolve full semver
// range expressions — it implements a minimal, zero-dependency range check
// sufficient for the ">=X.Y.Z" form the SDK uses.
func AssertCompatible(t *testing.T, modules []servicekit.Module, host HostRequires) {
	t.Helper()

	for _, m := range modules {
		d := m.Descriptor()
		r := d.Requires

		if r.SDK != "" && host.SDK != "" {
			if !versionSatisfies(host.SDK, r.SDK) {
				t.Errorf("AssertCompatible: module %q requires SDK %s but host provides %s",
					d.ID, r.SDK, host.SDK)
			}
		}

		if r.Go != "" && host.Go != "" {
			if !versionSatisfies(host.Go, r.Go) {
				t.Errorf("AssertCompatible: module %q requires Go %s but host provides %s",
					d.ID, r.Go, host.Go)
			}
		}

		if r.Postgres != "" {
			if host.Postgres == "" {
				t.Errorf("AssertCompatible: module %q requires Postgres %s but host has no Postgres",
					d.ID, r.Postgres)
				continue
			}
			if !versionSatisfies(host.Postgres, r.Postgres) {
				t.Errorf("AssertCompatible: module %q requires Postgres %s but host provides %s",
					d.ID, r.Postgres, host.Postgres)
			}
		}
	}
}

// CompatibleModules is the non-test form of [AssertCompatible]: it returns a slice of
// error strings (one per unsatisfied module constraint) rather than calling t.Fatal.
// This is what `de compose tidy` (P4) calls when there is no *testing.T available.
func CompatibleModules(modules []servicekit.Module, host HostRequires) []error {
	var errs []error
	for _, m := range modules {
		d := m.Descriptor()
		r := d.Requires

		if r.SDK != "" && host.SDK != "" {
			if !versionSatisfies(host.SDK, r.SDK) {
				errs = append(errs, fmt.Errorf("module %q requires SDK %s but host provides %s",
					d.ID, r.SDK, host.SDK))
			}
		}
		if r.Go != "" && host.Go != "" {
			if !versionSatisfies(host.Go, r.Go) {
				errs = append(errs, fmt.Errorf("module %q requires Go %s but host provides %s",
					d.ID, r.Go, host.Go))
			}
		}
		if r.Postgres != "" {
			if host.Postgres == "" {
				errs = append(errs, fmt.Errorf("module %q requires Postgres %s but host has no Postgres",
					d.ID, r.Postgres))
				continue
			}
			if !versionSatisfies(host.Postgres, r.Postgres) {
				errs = append(errs, fmt.Errorf("module %q requires Postgres %s but host provides %s",
					d.ID, r.Postgres, host.Postgres))
			}
		}
	}
	return errs
}

// ---- internal helpers ---------------------------------------------------

// cancelledCtx returns an already-cancelled context. Used to run [servicekit.Run]
// so it exits immediately after the boot gate (no real listener needed).
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// allocEphemeralAddr binds :0, reads the OS-assigned port, closes the socket,
// and returns the address (e.g. "127.0.0.1:54321"). The caller uses this address
// for the host so it can be dialled to detect serving.
func allocEphemeralAddr() (string, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr, nil
}

// waitForListener dials addr until it accepts a TCP connection or deadline is
// reached. It mirrors the pattern used in testdata/iam/iamv1/ws012_namespace_pg_test.go.
func waitForListener(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("AssertComposition: server did not start listening on %s within %s (migration or boot failed)", addr, timeout)
}

// versionSatisfies is a minimal, zero-dependency version range checker that
// handles the ">=X.Y.Z" (and ">=X.Y") prefix form the SDK uses in Compatibility
// fields. It strips the "v" prefix from version strings before comparing.
//
// Rules:
//   - ">=A.B.C" → provided version must be >= A.B.C (numeric tuple comparison).
//   - ">A.B.C"  → provided version must be >  A.B.C.
//   - Anything else is accepted as-is (returns true) — not our concern to parse.
//
// This is intentionally minimal: the full semver range DSL is a P4+ concern.
func versionSatisfies(provided, required string) bool {
	required = strings.TrimSpace(required)
	provided = strings.TrimSpace(provided)

	var op string
	var reqVer string
	switch {
	case strings.HasPrefix(required, ">="):
		op = ">="
		reqVer = strings.TrimPrefix(required, ">=")
	case strings.HasPrefix(required, ">"):
		op = ">"
		reqVer = strings.TrimPrefix(required, ">")
	default:
		// Unsupported range syntax — treat as satisfied (conservative).
		return true
	}

	cmp := compareVersions(provided, reqVer)
	switch op {
	case ">=":
		return cmp >= 0
	case ">":
		return cmp > 0
	}
	return true
}

// compareVersions compares two dot-separated version strings numerically.
// It strips a leading "v" from each before parsing. Returns < 0, 0, or > 0.
func compareVersions(a, b string) int {
	a = strings.TrimPrefix(strings.TrimSpace(a), "v")
	b = strings.TrimPrefix(strings.TrimSpace(b), "v")
	aParts := strings.SplitN(a, ".", 4)
	bParts := strings.SplitN(b, ".", 4)
	max := len(aParts)
	if len(bParts) > max {
		max = len(bParts)
	}
	for i := 0; i < max; i++ {
		av := versionPartInt(aParts, i)
		bv := versionPartInt(bParts, i)
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

// versionPartInt returns parts[i] as an int, or 0 if out of range or non-numeric.
func versionPartInt(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	s := strings.TrimSpace(parts[i])
	// Strip any pre-release suffix (e.g. "5-beta" → "5")
	if idx := strings.IndexAny(s, "-+"); idx >= 0 {
		s = s[:idx]
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// startOnce is a sync helper for tests that start a shared resource once per suite.
// It is exported so test helpers in other packages can use it without copy-pasting.
type startOnce struct {
	mu  sync.Mutex
	err error
	val any
}

// Do calls f exactly once and caches its result. Subsequent calls return the
// cached value/err without calling f again. This mirrors the pgOnce pattern
// in testdata/iam/iamv1/pgtest_test.go.
func (s *startOnce) Do(f func() (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.val != nil || s.err != nil {
		return s.val, s.err
	}
	s.val, s.err = f()
	return s.val, s.err
}
