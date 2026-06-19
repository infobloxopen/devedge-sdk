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

// TestEntBackend_NotYetImplemented confirms the ent path is cleanly blocked
// (Phase 4 hook) rather than half-generating.
func TestEntBackend_NotYetImplemented(t *testing.T) {
	requireTools(t, "apx", "buf")
	_, err := scaffold.Generate(context.Background(), scaffold.Options{
		Service: "orders", Resource: "Order", Backend: scaffold.BackendEnt,
		Dir: filepath.Join(t.TempDir(), "orders"), NoGenerate: true,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("expected ent backend to be rejected as not-yet-implemented, got: %v", err)
	}
}

// --- helpers ---

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
	for _, p := range []string{"protoc-gen-devedge-authz", "protoc-gen-svc", "protoc-gen-storage"} {
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
