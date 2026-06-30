package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/cmd/devedge-sdk/internal/scaffold"
)

// TestScaffold_GORM_BuildsAndPasses is the F028 end-to-end gate (AC-001, AC-005
// gorm, AC-007): scaffold a gorm service into a temp dir, then drive the generated
// project's REAL make targets — `make generate && make build && make test` (plus
// `go vet`) — which must all pass with ZERO hand-edits. Running the actual Makefile
// (not a copy of its recipe) is what guards against the dogfood-F-4 class of
// "harness compensates for a Makefile gap" regressions.
//
// It also exercises HEAD (not the module proxy): the generated go.mod requires
// the released SDK, but the test injects a local `replace` pointing at this
// checkout, and points buf at SDK plugins built from this checkout
// (DEVEDGE_SDK_PLUGIN_BIN). Real scaffolds emit no replace.
//
// Requires apx + buf + go + make on PATH and network access (buf dep update + go
// mod tidy). Skipped under -short.
func TestScaffold_GORM_BuildsAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold integration test in -short mode")
	}
	requireTools(t, "apx", "buf", "go", "make")

	sdkDir := sdkModuleDir(t)
	pluginBin := buildSDKPlugins(t, sdkDir)
	target := filepath.Join(t.TempDir(), "orders")

	var out bytes.Buffer
	m, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service:    "orders",
		Resource:   "Order",
		Backend:    scaffold.BackendGORM,
		Dir:        target,
		NoGenerate: true, // we run generate manually with the local replace + plugins
	}, &out)
	if err != nil {
		t.Fatalf("Generate: %v\n%s", err, out.String())
	}
	if m.Backend != scaffold.BackendGORM {
		t.Fatalf("backend = %q", m.Backend)
	}

	// Sanity: expected files present.
	for _, rel := range []string{
		"apx.yaml", "buf.yaml", "buf.gen.yaml", "go.mod", "Makefile", ".gitignore",
		"proto/orders/v1/orders.proto",
		"proto/infoblox/authz/v1/authz.proto",
		"proto/infoblox/field/v1/field.proto",
		// WS-012 composable shape: importable module/ unit + thin cmd/<svc> host.
		"module/module.go",
		"module/migrations/README.md",
		"cmd/orders/main.go",
		"cmd/orders/orders_smoke_test.go",
		".github/workflows/apx-release.yml",
		// Container image: distroless/static build + GHCR publish-on-merge workflow.
		"Dockerfile",
		".dockerignore",
		".github/workflows/image.yml",
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Fatalf("expected scaffold file %s: %v", rel, err)
		}
	}

	injectLocalReplace(t, target, sdkDir)
	generate(t, target, pluginBin)

	makeTarget(t, target, pluginBin, "build")
	run(t, target, nil, "go", "vet", "./...") // no `vet` make target; check directly
	makeTarget(t, target, pluginBin, "test")  // includes the generated smoke test (AC-007)
}

// TestScaffold_ENT_BuildsAndPasses is the F028 Phase-4 end-to-end gate (AC-005
// ent, in parity with the gorm gate above): scaffold an ent service into a temp
// dir, drive the real `make generate` (whose ent recipe is the FULL two-step buf
// generate → entc client gen), then `make build && make test` (plus `go vet`) must
// all pass with ZERO hand-written ent wiring — the persistence layer is F027's
// generated New<R>EntRepository adapter, not a hand-authored ent_wiring.go.
//
// Like the gorm test it exercises HEAD: a local SDK `replace` + SDK plugins built
// from this checkout (incl. protoc-gen-ent), and the real Makefile targets.
// Requires apx + buf + go + make + network.
func TestScaffold_ENT_BuildsAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold integration test in -short mode")
	}
	requireTools(t, "apx", "buf", "go", "make")

	sdkDir := sdkModuleDir(t)
	pluginBin := buildSDKPlugins(t, sdkDir)
	target := filepath.Join(t.TempDir(), "orders")

	var out bytes.Buffer
	m, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service:    "orders",
		Resource:   "Order",
		Backend:    scaffold.BackendEnt,
		Dir:        target,
		NoGenerate: true, // we run the two-step generate manually with the local replace + plugins
	}, &out)
	if err != nil {
		t.Fatalf("Generate: %v\n%s", err, out.String())
	}
	if m.Backend != scaffold.BackendEnt {
		t.Fatalf("backend = %q", m.Backend)
	}

	// Sanity: expected ent files present (tools.go pins entc; no gorm go.mod dep).
	for _, rel := range []string{
		"apx.yaml", "buf.yaml", "buf.gen.yaml", "go.mod", "Makefile", ".gitignore", "tools.go",
		"proto/orders/v1/orders.proto",
		"proto/infoblox/authz/v1/authz.proto",
		"proto/infoblox/field/v1/field.proto",
		// WS-012 composable shape: importable module/ unit + thin cmd/<svc> host.
		"module/module.go",
		"module/migrations/README.md",
		"cmd/orders/main.go",
		"cmd/orders/orders_smoke_test.go",
		".github/workflows/apx-release.yml",
		// Container image: distroless/static build + GHCR publish-on-merge workflow.
		"Dockerfile",
		".dockerignore",
		".github/workflows/image.yml",
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Fatalf("expected scaffold file %s: %v", rel, err)
		}
	}

	// Parity assertion #1 (boundary / AC-004): an ent service must be gorm-free.
	// The ent buf.gen excludes protoc-gen-storage, so no GORM model is generated
	// and gorm must NOT appear in the consumer go.mod (it's the storage backend's
	// dep, not ent's).
	consumerGoMod := readFile(t, filepath.Join(target, "go.mod"))
	if strings.Contains(consumerGoMod, "gorm.io/") {
		t.Errorf("ent consumer go.mod must be gorm-free, but references gorm:\n%s", consumerGoMod)
	}
	if !strings.Contains(consumerGoMod, "entgo.io/ent") {
		t.Errorf("ent consumer go.mod missing entgo.io/ent:\n%s", consumerGoMod)
	}

	injectLocalReplace(t, target, sdkDir)
	generateEnt(t, target, pluginBin)

	// Parity assertion #2 (AC-005 ent): ZERO hand-written ent wiring. The repo
	// adapter is generated by protoc-gen-ent (F027). Assert the generated file
	// exists and that no hand-written ent_wiring.go was needed.
	if _, err := os.Stat(filepath.Join(target, "gen/ordersv1/order_repo.ent.go")); err != nil {
		t.Fatalf("expected generated ent repository adapter gen/ordersv1/order_repo.ent.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "server/ent_wiring.go")); err == nil {
		t.Fatalf("found hand-written server/ent_wiring.go; the ent backend must need ZERO hand-written wiring (F027)")
	}

	makeTarget(t, target, pluginBin, "build")
	run(t, target, nil, "go", "vet", "./...") // no `vet` make target; check directly
	makeTarget(t, target, pluginBin, "test")  // includes the generated smoke test (AC-007 ent)
}

// TestScaffold_GORM_Aggregate_BuildsAndPasses is the F034 (scaffold aggregate +
// outbox) end-to-end gate on GORM: scaffold an AGGREGATE service (--aggregate),
// then drive the generated project's REAL `make generate && make build && make
// test` (plus `go vet`). An aggregate scaffold wires, OUT OF THE BOX, more than
// Tier-1 CRUD: a gormtx.GormTxRunner, a persistence.AggregateRepository over the
// generated LoadOrderAggregateGorm graph-load primitive, and a transactional
// outbox (events.Publisher + events.Dispatcher over the reusable gormtx outbox +
// idempotency stores, plus a RunRetention hook) — all of which must compile and
// the smoke test still pass with ZERO hand-edits.
//
// Like the non-aggregate gorm gate it exercises HEAD (local SDK replace + plugins).
// Requires apx + buf + go + make + network. Skipped under -short.
func TestScaffold_GORM_Aggregate_BuildsAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold aggregate integration test in -short mode")
	}
	requireTools(t, "apx", "buf", "go", "make")

	sdkDir := sdkModuleDir(t)
	pluginBin := buildSDKPlugins(t, sdkDir)
	target := filepath.Join(t.TempDir(), "orders")

	var out bytes.Buffer
	if _, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service:    "orders",
		Resource:   "Order",
		Backend:    scaffold.BackendGORM,
		Dir:        target,
		Aggregate:  true,
		NoGenerate: true,
	}, &out); err != nil {
		t.Fatalf("Generate: %v\n%s", err, out.String())
	}

	// The aggregate proto declares the root + an owned member and vendors the
	// SDK-owned ddd annotation the aggregate/member options live in.
	proto := readFile(t, filepath.Join(target, "proto/orders/v1/orders.proto"))
	for _, want := range []string{
		"option (infoblox.ddd.v1.aggregate) = {root: true};",
		"option (infoblox.ddd.v1.member) = {root: \"Order\"};",
		"message OrderItem {",
	} {
		if !strings.Contains(proto, want) {
			t.Fatalf("aggregate proto missing %q:\n%s", want, proto)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "proto/infoblox/ddd/v1/ddd.proto")); err != nil {
		t.Fatalf("aggregate scaffold must vendor the ddd annotation mirror: %v", err)
	}

	// The generated main wires the aggregate + outbox machinery (gated on aggregate).
	mainGo := readFile(t, filepath.Join(target, "cmd/orders/main.go"))
	for _, want := range []string{
		"gormtx.NewGormTxRunner(db)",
		"persistence.NewGenericAggregateRepository",
		"LoadOrderAggregateGorm",
		"events.NewOutboxPublisher",
		"events.NewDispatcher",
		"gormtx.RunRetention",
	} {
		if !strings.Contains(mainGo, want) {
			t.Fatalf("aggregate main.go missing wiring %q:\n%s", want, mainGo)
		}
	}

	injectLocalReplace(t, target, sdkDir)
	generate(t, target, pluginBin)

	// The generated graph-load primitive is only emitted for a root WITH members;
	// assert it landed (so the aggregate wiring in main compiles against it).
	if _, err := os.Stat(filepath.Join(target, "gen/ordersv1/orders.storage.go")); err != nil {
		t.Fatalf("expected generated storage file: %v", err)
	}

	makeTarget(t, target, pluginBin, "build") // compiles the aggregate + outbox wiring
	run(t, target, nil, "go", "vet", "./...") // no `vet` make target; check directly
	makeTarget(t, target, pluginBin, "test")  // smoke test still green (AC-007)
}

// TestScaffold_ENT_Aggregate_BuildsAndPasses is the ent twin of the gorm aggregate
// gate: an ent aggregate scaffold wires the generated NewEntTxRunner +
// AggregateRepository over the generated LoadOrderAggregate primitive, plus a
// transactional outbox on the SDK's engine-free dev stores (the durable SQL-backed
// ent outbox store is a documented follow-up — see testdata/iam). It must compile
// and pass via the real two-step ent `make generate` → `make build && make test`.
// Requires apx + buf + go + make + network. Skipped under -short.
func TestScaffold_ENT_Aggregate_BuildsAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold aggregate integration test in -short mode")
	}
	requireTools(t, "apx", "buf", "go", "make")

	sdkDir := sdkModuleDir(t)
	pluginBin := buildSDKPlugins(t, sdkDir)
	target := filepath.Join(t.TempDir(), "orders")

	var out bytes.Buffer
	if _, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service:    "orders",
		Resource:   "Order",
		Backend:    scaffold.BackendEnt,
		Dir:        target,
		Aggregate:  true,
		NoGenerate: true,
	}, &out); err != nil {
		t.Fatalf("Generate: %v\n%s", err, out.String())
	}

	// ent aggregate main wires the generated NewEntTxRunner + AggregateRepository +
	// the (dev-store) outbox. It must stay gorm-free.
	mainGo := readFile(t, filepath.Join(target, "cmd/orders/main.go"))
	for _, want := range []string{
		"ordersv1.NewEntTxRunner(client)",
		"persistence.NewGenericAggregateRepository",
		"LoadOrderAggregate(ctx, client",
		"events.NewOutboxPublisher",
		"events.NewDispatcher",
	} {
		if !strings.Contains(mainGo, want) {
			t.Fatalf("ent aggregate main.go missing wiring %q:\n%s", want, mainGo)
		}
	}
	if strings.Contains(mainGo, "gorm") {
		t.Errorf("ent aggregate main.go must be gorm-free:\n%s", mainGo)
	}

	injectLocalReplace(t, target, sdkDir)
	generateEnt(t, target, pluginBin)

	makeTarget(t, target, pluginBin, "build")
	run(t, target, nil, "go", "vet", "./...")
	makeTarget(t, target, pluginBin, "test")
}

// TestScaffold_GORM_AuthzGateRegression is AC-002 (T-303): delete one authz rule
// from the example proto, regenerate (via the real `make generate`), and the server
// must FAIL to boot with the completeness-gate error.
func TestScaffold_GORM_AuthzGateRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping authz-gate regression test in -short mode")
	}
	requireTools(t, "apx", "buf", "go", "make")

	sdkDir := sdkModuleDir(t)
	pluginBin := buildSDKPlugins(t, sdkDir)
	target := filepath.Join(t.TempDir(), "orders")

	var out bytes.Buffer
	if _, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service: "orders", Resource: "Order", Backend: scaffold.BackendGORM,
		Dir: target, NoGenerate: true,
	}, &out); err != nil {
		t.Fatalf("Generate: %v\n%s", err, out.String())
	}

	// Strip the authz rule from GetOrder.
	protoPath := filepath.Join(target, "proto/orders/v1/orders.proto")
	data, err := os.ReadFile(protoPath)
	if err != nil {
		t.Fatal(err)
	}
	const ruleLine = `    option (infoblox.authz.v1.rule) = {verb: "read", resource: "orders"};` + "\n"
	s := string(data)
	idx := strings.Index(s, ruleLine)
	if idx < 0 {
		t.Fatalf("could not find GetOrder authz rule line to remove in:\n%s", s)
	}
	s = s[:idx] + s[idx+len(ruleLine):] // remove the FIRST read rule (GetOrder)
	if err := os.WriteFile(protoPath, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}

	injectLocalReplace(t, target, sdkDir)
	generate(t, target, pluginBin)

	// The smoke test drives runHost -> servicekit.Run -> server.Serve, whose
	// fail-closed union completeness gate must now reject the orphan method at boot.
	cmd := exec.Command("go", "test", "./cmd/orders/", "-run", "TestSmoke")
	cmd.Dir = target
	combined, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected smoke test to FAIL (authz gate), but it passed:\n%s", combined)
	}
	if !strings.Contains(string(combined), "undeclared") {
		t.Fatalf("expected completeness-gate 'undeclared' error, got:\n%s", combined)
	}
}

// TestScaffold_Boundary asserts AC-004 / the engine-dep-leak failure mode without
// any toolchain: the generated tree git-ignores the generated code and pulls gorm
// into the CONSUMER go.mod, while the SDK go.mod stays gorm-free.
func TestScaffold_Boundary(t *testing.T) {
	requireTools(t, "apx") // apx init writes .gitignore/apx.yaml; no buf/network needed
	sdkDir := sdkModuleDir(t)
	target := filepath.Join(t.TempDir(), "orders")

	var out bytes.Buffer
	if _, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service: "orders", Resource: "Order", Backend: scaffold.BackendGORM,
		Dir: target, NoGenerate: true,
	}, &out); err != nil {
		t.Fatalf("Generate: %v\n%s", err, out.String())
	}

	gitignore := readFile(t, filepath.Join(target, ".gitignore"))
	if !strings.Contains(gitignore, "/gen/") {
		t.Errorf(".gitignore does not ignore /gen/ (generated code must be untracked):\n%s", gitignore)
	}

	consumerGoMod := readFile(t, filepath.Join(target, "go.mod"))
	if !strings.Contains(consumerGoMod, "gorm.io/gorm") {
		t.Errorf("consumer go.mod missing gorm.io/gorm:\n%s", consumerGoMod)
	}

	// Clean-core guarantee: gorm is confined to the persistence/gormtx adapter
	// package (the GORM analogue of persistence/entrepo for ent). The core trees
	// (persistence, authz, grpcauthz) must never import it. We assert this at the
	// import level rather than on the SDK go.mod, because the go.mod legitimately
	// carries both adapter engines (entgo.io/ent and gorm.io/gorm) as deps of the
	// sibling adapter packages — exactly as it already does for ent. Both engines
	// are policed symmetrically: a leak of EITHER ORM into the core is a defect.
	assertCoreImportFree(t, sdkDir, "gorm.io/")
	assertCoreImportFree(t, sdkDir, "entgo.io/")

	// The public proto must contain no engine options (AC-004 / apx policy guardrail).
	proto := readFile(t, filepath.Join(target, "proto/orders/v1/orders.proto"))
	if strings.Contains(proto, "gorm.") {
		t.Errorf("public proto must not contain engine options (gorm.*):\n%s", proto)
	}
}

// assertCoreImportFree fails if the engine identified by importPrefix (e.g.
// "gorm.io/") is reachable from the clean core — the top-level persistence tree
// plus the whole authz tree (which includes authz/grpcauthz). The engine
// adapters live in persistence/{entrepo,gormtx}; the policy/persistence core must
// never reach an ORM, directly OR transitively through one of those adapters.
//
// The check is deliberately over the FULL transitive dependency closure of the
// core roots (`go list -deps`), not over each core package's direct imports.
// That distinction has teeth: a core package that imports the gormtx/entrepo
// adapter would NOT name "gorm.io/" in its own import list (it names
// ".../persistence/gormtx"), so a direct-import-only check would miss it and the
// adapter would launder the ORM into the core. Because the adapter packages are
// never in the core's closure in a clean tree, any appearance of importPrefix in
// the closure is an unambiguous leak — whether direct or via an adapter.
func assertCoreImportFree(t *testing.T, sdkDir, importPrefix string) {
	t.Helper()
	// 1) Hard gate: importPrefix must not appear anywhere in the core's
	//    transitive dependency closure.
	cmd := exec.Command("go", "list", "-deps", "./persistence", "./authz/...")
	cmd.Dir = sdkDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list core deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		if strings.Contains(dep, importPrefix) {
			t.Errorf("clean core (persistence + authz/...) must not depend on %q (engine leak): %q is in the transitive dependency closure", importPrefix, dep)
		}
	}

	// 2) Best-effort breadcrumb: name the first-party core package that pulls in
	//    the engine — directly, or via the sanctioned adapter subpackages. This
	//    only produces a diagnostic; the gate above is the authority.
	cmd = exec.Command("go", "list", "-deps", "-f",
		"{{.ImportPath}} {{join .Imports \" \"}}",
		"./persistence", "./authz/...")
	cmd.Dir = sdkDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return // gate above already ran; the detail listing is optional.
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg := fields[0]
		if !strings.HasPrefix(pkg, "github.com/infobloxopen/devedge-sdk/") {
			continue // only police first-party core packages
		}
		// The adapter packages legitimately import the engine; flag them only
		// when they appear AS A DEPENDENCY of a core root (i.e. some core
		// package reached them), which the gate above has already failed on.
		if strings.Contains(pkg, "/persistence/entrepo") || strings.Contains(pkg, "/persistence/gormtx") {
			t.Errorf("clean core reached engine adapter %s (it should never be in the core's dependency closure)", pkg)
			continue
		}
		for _, imp := range fields[1:] {
			if strings.Contains(imp, importPrefix) ||
				strings.Contains(imp, "/persistence/entrepo") ||
				strings.Contains(imp, "/persistence/gormtx") {
				t.Errorf("clean-core package %s must not import %q (engine leak)", pkg, imp)
			}
		}
	}
}

// TestScaffold_ENT_Boundary is the ent counterpart of TestScaffold_Boundary
// (AC-004): the rendered ent tree git-ignores generated code, pins entc via
// tools.go, and keeps the public proto + SDK go.mod engine-free. No buf/network
// needed (render only).
func TestScaffold_ENT_Boundary(t *testing.T) {
	requireTools(t, "apx") // apx init writes .gitignore/apx.yaml
	sdkDir := sdkModuleDir(t)
	target := filepath.Join(t.TempDir(), "orders")

	var out bytes.Buffer
	if _, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service: "orders", Resource: "Order", Backend: scaffold.BackendEnt,
		Dir: target, NoGenerate: true,
	}, &out); err != nil {
		t.Fatalf("Generate: %v\n%s", err, out.String())
	}

	gitignore := readFile(t, filepath.Join(target, ".gitignore"))
	if !strings.Contains(gitignore, "/gen/") {
		t.Errorf(".gitignore does not ignore /gen/ (generated ent code, incl. gen/ent, must be untracked):\n%s", gitignore)
	}

	consumerGoMod := readFile(t, filepath.Join(target, "go.mod"))
	if !strings.Contains(consumerGoMod, "entgo.io/ent") {
		t.Errorf("ent consumer go.mod missing entgo.io/ent:\n%s", consumerGoMod)
	}
	if strings.Contains(consumerGoMod, "gorm.io/") {
		t.Errorf("ent consumer go.mod must be gorm-free:\n%s", consumerGoMod)
	}

	// tools.go pins entc so `go mod tidy` keeps the entc-only deps.
	tools := readFile(t, filepath.Join(target, "tools.go"))
	if !strings.Contains(tools, "entgo.io/ent/cmd/ent") {
		t.Errorf("tools.go must pin entc (entgo.io/ent/cmd/ent):\n%s", tools)
	}

	// The public proto must contain no engine options (AC-004 / apx guardrail).
	proto := readFile(t, filepath.Join(target, "proto/orders/v1/orders.proto"))
	if strings.Contains(proto, "gorm.") || strings.Contains(proto, "ent.") {
		t.Errorf("public proto must not contain engine options:\n%s", proto)
	}

	// Clean-core guarantee (import level): the core trees never import an ORM; the
	// engine adapters live in persistence/{entrepo,gormtx}. See the note on
	// assertCoreImportFree in TestScaffold_Boundary. Police both engines so the ent
	// boundary test also guards against an ent leak into the core.
	assertCoreImportFree(t, sdkDir, "gorm.io/")
	assertCoreImportFree(t, sdkDir, "entgo.io/")
}

// TestScaffold_APXGovernance is F028 Phase 5 (T-501/T-502; AC-003, AC-004 at the
// contract level, AC-006): on a freshly scaffolded service's PUBLIC proto, every
// apx governance gate passes —
//
//	apx config validate          (AC-006: the emitted apx.yaml is valid)
//	apx lint                     (AC-003: STANDARD lint clean)
//	apx breaking --against HEAD  (AC-003: a new API has nothing to break against;
//	                              the committed proto compared to itself = the
//	                              trivial "empty baseline" — see note below)
//	apx release prepare --dry-run (AC-003: a v1.0.0-alpha.1 experimental release
//	                              would be prepared successfully)
//	apx policy check             (AC-004/G-003: forbidden_proto_options ^gorm\. —
//	                              proves no engine leak into the published surface)
//
// "empty baseline" note: `apx breaking --against <empty dir>` fails inside buf
// ("Module had no .proto files"), so the correct trivial baseline for a brand-new
// API is the committed proto compared against itself (HEAD) — there is no prior
// version, so nothing can break. That is exactly what the generated
// apx-release.yml CI does (`apx breaking --against HEAD^`).
//
// The lint and breaking gates are driven through the generated project's REAL
// `make api-lint` / `make api-breaking` targets (so the Makefile's api-* wrappers,
// incl. api-breaking's git-repo guard, are themselves under test); config-validate,
// release-prepare --dry-run, and policy-check have no make wrapper and are invoked
// on apx directly.
//
// Requires apx + buf + git + make + network (buf dep update). Skipped under -short.
func TestScaffold_APXGovernance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping apx governance integration test in -short mode")
	}
	requireTools(t, "apx", "buf", "git", "make")

	target := filepath.Join(t.TempDir(), "orders")
	var out bytes.Buffer
	if _, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service: "orders", Resource: "Order", Backend: scaffold.BackendGORM,
		Dir: target, NoGenerate: true,
	}, &out); err != nil {
		t.Fatalf("Generate: %v\n%s", err, out.String())
	}

	// apx lint/breaking compile the proto via buf, which needs the googleapis
	// imports resolved into buf.lock first.
	run(t, target, nil, "buf", "dep", "update")

	// Commit a baseline so `apx breaking --against HEAD` has a ref to diff.
	run(t, target, nil, "git", "init", "-q")
	run(t, target, nil, "git", "add", "-A")
	run(t, target, gitCommitEnv(), "git", "commit", "-q", "-m", "scaffold baseline")

	// AC-006: the emitted apx.yaml validates against apx's canonical schema.
	run(t, target, nil, "apx", "config", "validate")

	// AC-003: lint clean — through the generated `make api-lint` wrapper.
	makeTarget(t, target, "", "api-lint")

	// AC-003: no breaking changes vs the initial baseline (proto unchanged) —
	// through `make api-breaking`, which also exercises its git-repo guard (the
	// baseline commit above satisfies it).
	makeTarget(t, target, "", "api-breaking")

	// AC-003: a versioned experimental release would prepare successfully.
	// NOTE: this emits a NON-FATAL go_package mismatch warning ("got
	// .../gen/ordersv1, expected .../proto/orders/v1"). It is unavoidable without
	// --strict: apx derives the expected go_package rigidly as <module>/<api-id>
	// (= .../proto/orders/v1), but the scaffold's generated Go must be a single
	// directory segment under gen/ (gen/ordersv1) so protoc-gen-ent's sibling
	// ent/ package lines up (see scaffold/model.go GoImportPath + tasks.md). The
	// command exits 0; we assert that here. Do NOT pass --strict.
	run(t, target, nil, "apx", "release", "prepare", "proto/orders/v1",
		"--version", "v1.0.0-alpha.1", "--lifecycle", "experimental", "--dry-run")

	// AC-004 / G-003: the public surface carries no engine options
	// (forbidden_proto_options: ^gorm\.). This is the guardrail proving no engine
	// leak into the published contract.
	run(t, target, nil, "apx", "policy", "check")
}

// --- helpers ---

// gitCommitEnv returns an env with a deterministic git identity so `git commit`
// works in CI sandboxes that have no global git user configured.
func gitCommitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=scaffold-test", "GIT_AUTHOR_EMAIL=scaffold@test.local",
		"GIT_COMMITTER_NAME=scaffold-test", "GIT_COMMITTER_EMAIL=scaffold@test.local",
	)
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not on PATH; skipping", n)
		}
	}
}

// sdkModuleDir returns the ROOT devedge-sdk module dir. It names the module
// explicitly (not a bare `go list -m`): the repo is now a multi-module workspace
// (WS-011), and under go.work a bare `go list -m -f {{.Dir}}` prints a line per
// workspace module, so querying the root module by path is the only reliable way
// to get just the root tree.
func sdkModuleDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", sdkModulePath).Output()
	if err != nil {
		t.Fatalf("locate SDK module dir: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// sdkModulePath is the root SDK module path (the import path of its top-level
// packages). The nested adapter modules live UNDER it (e.g. observability/otel).
const sdkModulePath = "github.com/infobloxopen/devedge-sdk"

// buildSDKPlugins builds the SDK codegen plugins from this checkout into a temp
// bin dir, so buf generate exercises HEAD.
func buildSDKPlugins(t *testing.T, sdkDir string) string {
	t.Helper()
	bin := t.TempDir()
	for _, p := range []string{"protoc-gen-devedge-authz", "protoc-gen-svc", "protoc-gen-storage", "protoc-gen-ent", "openapiv2to3"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, p), "./cmd/"+p)
		cmd.Dir = sdkDir
		if outb, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", p, err, outb)
		}
	}
	return bin
}

// injectLocalReplace points the generated project's SDK requires at THIS
// checkout so the E2E exercises HEAD, not the module proxy. It replaces both the
// root module AND every nested adapter module the generated go.mod now requires
// (WS-011: the generated main imports .../observability/otel, which is its own
// module — its require must resolve to this working tree too, exactly as the root
// does). Real scaffolds emit no replace. Each nested module lives at <sdkDir>/<rel>
// where <rel> is its module path minus the root prefix.
func injectLocalReplace(t *testing.T, target, sdkDir string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(target, "go.mod"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	repl := "\nreplace " + sdkModulePath + " => " + sdkDir + "\n"
	for _, rel := range nestedAdapterModules {
		repl += "replace " + sdkModulePath + "/" + rel + " => " + filepath.Join(sdkDir, rel) + "\n"
	}
	if _, err := f.WriteString(repl); err != nil {
		t.Fatal(err)
	}
}

// nestedAdapterModules lists the SDK's nested adapter modules (paths relative to
// the root module path) that a generated service may require. As P1/P2 split out
// config/koanf, events/kafkabus, persistence/* — add them here so the E2E keeps
// resolving every required adapter module locally.
//
// P2: every generated service requires a persistence adapter (gorm → gormtx, ent
// → entrepo). Both are listed so injectLocalReplace adds a local replace for each;
// a replace for a module the generated go.mod does not require is harmless (Go
// ignores an unused replace), so listing both keeps the helper backend-agnostic.
var nestedAdapterModules = []string{
	"observability/otel",
	"persistence/gormtx",
	"persistence/entrepo",
}

// makeTarget runs the generated project's REAL `make <args...>` target in target,
// returning combined output and failing on error. Driving the actual Makefile (not
// a hand-rolled copy of its recipe) is deliberate: it is the only way the test can
// catch a Makefile-level regression. The dogfood-F-4 bug hid for exactly the
// opposite reason — the old helpers ran `buf dep update` that the Makefile's
// generate target lacked, so the harness silently "fixed" the very gap under test.
//
// When pluginBin != "", the SDK codegen plugins built from this checkout are
// prepended to PATH so the `generate` recipe's `buf generate` resolves HEAD's
// plugins. A real user's `make tools` installs those plugins at the pinned SDK
// version; the test supplies them directly and skips `make tools` (build/test/
// api-* targets never invoke tools once gen/ exists).
func makeTarget(t *testing.T, target, pluginBin string, args ...string) []byte {
	t.Helper()
	var env []string
	if pluginBin != "" {
		gobin := filepath.Join(build0(t, "GOPATH"), "bin")
		pathEnv := "PATH=" + pluginBin + string(os.PathListSeparator) + gobin +
			string(os.PathListSeparator) + os.Getenv("PATH")
		env = append(os.Environ(), pathEnv)
	}
	cmd := exec.Command("make", args...)
	cmd.Dir = target
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// generate drives the generated Makefile's REAL `generate` target (gorm) with the
// local SDK plugins on PATH. buf.lock is absent here (the scaffold ran with
// NoGenerate), so the target's own guarded `buf dep update` must create it before
// `buf generate` — exactly the dogfood-F-4 path, now exercised through the Makefile.
func generate(t *testing.T, target, pluginBin string) {
	t.Helper()
	makeTarget(t, target, pluginBin, "generate")
}

// generateEnt drives the SAME real `make generate` target for the ent backend (its
// recipe is the buf→entc two-step: guarded buf dep update → buf generate → go get
// seed → go generate ./gen/ent → go mod tidy). It additionally asserts the
// cold-start prints none of the alarming "#4 fix" noise — and because it runs the
// actual Makefile recipe, that guard now validates the real cold-start a fresh
// clone runs, not a copy that could drift from it.
func generateEnt(t *testing.T, target, pluginBin string) {
	t.Helper()
	out := makeTarget(t, target, pluginBin, "generate")
	for _, scary := range []string{"Could not read from remote repository", "cannot find module providing package"} {
		if strings.Contains(string(out), scary) {
			t.Fatalf("ent `make generate` cold-start printed alarming output %q (the #4 fix must keep it clean):\n%s", scary, out)
		}
	}
}

func build0(t *testing.T, env string) string {
	t.Helper()
	out, err := exec.Command("go", "env", env).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", env, err)
	}
	return strings.TrimSpace(string(out))
}

func run(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
