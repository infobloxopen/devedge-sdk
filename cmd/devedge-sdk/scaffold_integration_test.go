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
// gorm, AC-007): scaffold a gorm service into a temp dir, then
// `buf generate && go build ./... && go vet ./...` and the generated smoke test
// must all pass with ZERO hand-edits.
//
// It also exercises HEAD (not the module proxy): the generated go.mod requires
// the released SDK, but the test injects a local `replace` pointing at this
// checkout, and points buf at SDK plugins built from this checkout
// (DEVEDGE_SDK_PLUGIN_BIN). Real scaffolds emit no replace.
//
// Requires apx + buf + go on PATH and network access (buf dep update + go mod
// tidy). Skipped under -short.
func TestScaffold_GORM_BuildsAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold integration test in -short mode")
	}
	requireTools(t, "apx", "buf", "go")

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
		"server/main.go",
		"server/orders_smoke_test.go",
		".github/workflows/apx-release.yml",
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Fatalf("expected scaffold file %s: %v", rel, err)
		}
	}

	injectLocalReplace(t, target, sdkDir)
	generate(t, target, pluginBin)

	run(t, target, nil, "go", "build", "./...")
	run(t, target, nil, "go", "vet", "./...")
	run(t, target, nil, "go", "test", "./...") // includes the generated smoke test (AC-007)
}

// TestScaffold_ENT_BuildsAndPasses is the F028 Phase-4 end-to-end gate (AC-005
// ent, in parity with the gorm gate above): scaffold an ent service into a temp
// dir, run the FULL ent two-step generate (buf generate → entc client gen), then
// `go build ./... && go vet ./...` and the generated smoke test must all pass
// with ZERO hand-written ent wiring — the persistence layer is F027's generated
// New<R>EntRepository adapter, not a hand-authored ent_wiring.go.
//
// Like the gorm test it exercises HEAD: a local SDK `replace` + SDK plugins built
// from this checkout (incl. protoc-gen-ent). Requires apx + buf + go + network.
func TestScaffold_ENT_BuildsAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold integration test in -short mode")
	}
	requireTools(t, "apx", "buf", "go")

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
		"server/main.go",
		"server/orders_smoke_test.go",
		".github/workflows/apx-release.yml",
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

	run(t, target, nil, "go", "build", "./...")
	run(t, target, nil, "go", "vet", "./...")
	run(t, target, nil, "go", "test", "./...") // includes the generated smoke test (AC-007 ent)
}

// TestScaffold_GORM_AuthzGateRegression is AC-002 (T-303): delete one authz rule
// from the example proto, regenerate, and the server must FAIL to boot with the
// completeness-gate error.
func TestScaffold_GORM_AuthzGateRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping authz-gate regression test in -short mode")
	}
	requireTools(t, "apx", "buf", "go")

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

	// The smoke test calls newServer, which must now fail at Register (boot gate).
	cmd := exec.Command("go", "test", "./server/", "-run", "TestSmoke")
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

	sdkGoMod := readFile(t, filepath.Join(sdkDir, "go.mod"))
	if strings.Contains(sdkGoMod, "gorm.io/") {
		t.Errorf("SDK go.mod must stay engine-free but references gorm:\n%s", sdkGoMod)
	}

	// The public proto must contain no engine options (AC-004 / apx policy guardrail).
	proto := readFile(t, filepath.Join(target, "proto/orders/v1/orders.proto"))
	if strings.Contains(proto, "gorm.") {
		t.Errorf("public proto must not contain engine options (gorm.*):\n%s", proto)
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

	sdkGoMod := readFile(t, filepath.Join(sdkDir, "go.mod"))
	if strings.Contains(sdkGoMod, "gorm.io/") {
		t.Errorf("SDK go.mod must stay engine-free but references gorm:\n%s", sdkGoMod)
	}
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
// Requires apx + buf + git + go + network (buf dep update). Skipped under -short.
func TestScaffold_APXGovernance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping apx governance integration test in -short mode")
	}
	requireTools(t, "apx", "buf", "git")

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

	// AC-003: lint clean.
	run(t, target, nil, "apx", "lint")

	// AC-003: no breaking changes vs the initial baseline (proto unchanged).
	run(t, target, nil, "apx", "breaking", "--against", "HEAD")

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

// sdkModuleDir returns the devedge-sdk module root (two levels up from this
// package, cmd/devedge-sdk).
func sdkModuleDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("locate SDK module dir: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// buildSDKPlugins builds the SDK codegen plugins from this checkout into a temp
// bin dir, so buf generate exercises HEAD.
func buildSDKPlugins(t *testing.T, sdkDir string) string {
	t.Helper()
	bin := t.TempDir()
	for _, p := range []string{"protoc-gen-devedge-authz", "protoc-gen-svc", "protoc-gen-storage", "protoc-gen-ent"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, p), "./cmd/"+p)
		cmd.Dir = sdkDir
		if outb, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", p, err, outb)
		}
	}
	return bin
}

func injectLocalReplace(t *testing.T, target, sdkDir string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(target, "go.mod"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("\nreplace github.com/infobloxopen/devedge-sdk => " + sdkDir + "\n"); err != nil {
		t.Fatal(err)
	}
}

// generate runs buf dep update + buf generate (with the local SDK plugins on
// PATH) + go mod tidy in target.
func generate(t *testing.T, target, pluginBin string) {
	t.Helper()
	gobin := filepath.Join(build0(t, "GOPATH"), "bin")
	pathEnv := "PATH=" + pluginBin + string(os.PathListSeparator) + gobin +
		string(os.PathListSeparator) + os.Getenv("PATH")
	env := append(os.Environ(), pathEnv)
	run(t, target, env, "buf", "dep", "update")
	run(t, target, env, "buf", "generate")
	run(t, target, nil, "go", "mod", "tidy")
}

// generateEnt runs the ent two-step generate (buf generate then entc client gen)
// in target, mirroring the generated Makefile's ent `generate` target EXACTLY
// (so the cold-start it validates is the one a fresh clone actually runs):
//
//	buf dep update → buf generate →
//	go get <entc tool + SDK pkgs> (seed go.sum WITHOUT building the module) →
//	go generate ./gen/ent (entc client) → go mod tidy.
//
// The `go get` (not `go mod tidy -e`) is the #4 fix: tidy -e tolerated the
// not-yet-generated gen/ent/* imports but printed an alarming `fatal: Could not
// read from remote repository`; `go get` of the exact deps never builds the
// module, so the cold-start is CLEAN. Output is asserted clean by the caller.
func generateEnt(t *testing.T, target, pluginBin string) {
	t.Helper()
	gobin := filepath.Join(build0(t, "GOPATH"), "bin")
	pathEnv := "PATH=" + pluginBin + string(os.PathListSeparator) + gobin +
		string(os.PathListSeparator) + os.Getenv("PATH")
	env := append(os.Environ(), pathEnv)
	run(t, target, env, "buf", "dep", "update")
	run(t, target, env, "buf", "generate")
	// Seed the entc toolchain + the SDK packages the schema/adapter import, into
	// go.sum, without building this module — so it never trips over the
	// not-yet-generated gen/ent/* packages (the #4 cold-start fix).
	runNoScaryOutput(t, target, env, "go", "get",
		"entgo.io/ent/cmd/ent",
		"github.com/infobloxopen/devedge-sdk/persistence/entrepo",
		"github.com/infobloxopen/devedge-sdk/middleware",
		"github.com/infobloxopen/devedge-sdk/persistence/resourcename",
	)
	run(t, target, env, "go", "generate", "./gen/ent")
	run(t, target, nil, "go", "mod", "tidy")
}

// runNoScaryOutput runs a command, fails on error, AND fails if the combined
// output contains the alarming cold-start noise the #4 fix eliminates — a guard
// that the clean cold-start does not regress back into fake "fatal" errors.
func runNoScaryOutput(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	for _, scary := range []string{"Could not read from remote repository", "cannot find module providing package"} {
		if strings.Contains(string(out), scary) {
			t.Fatalf("cold-start %s %s printed alarming output %q (the #4 fix must keep it clean):\n%s",
				name, strings.Join(args, " "), scary, out)
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
