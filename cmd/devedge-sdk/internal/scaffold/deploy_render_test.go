package scaffold

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderDeploy_EmitsBothTargets is the scaffold-side F038 gate: rendering the
// default deploy targets into a generated tree writes BOTH targets' file sets, the
// k8s output is Flux glue (HelmRelease + OCIRepository + values) and carries NO
// editable chart, and the env names trace to the service's config prefix (no drift).
func TestRenderDeploy_EmitsBothTargets(t *testing.T) {
	m, err := Options{
		Service:  "orders",
		Resource: "Order",
		Backend:  BackendGORM,
		Dir:      t.TempDir(),
	}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	dir := t.TempDir()
	if err := renderDeploy(dir, m); err != nil {
		t.Fatalf("renderDeploy: %v", err)
	}

	// Both targets' files are present.
	wantFiles := []string{
		"deploy/k8s/helmrelease.yaml",
		"deploy/k8s/oci-repository.yaml",
		"deploy/k8s/values.yaml",
		"deploy/k8s/README.md",
		"deploy/compose/docker-compose.yml",
	}
	for _, rel := range wantFiles {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected deploy file %s: %v", rel, err)
		}
	}

	// k8s output: HelmRelease + OCIRepository + values, NO editable chart.
	hr := readDeployFile(t, dir, "deploy/k8s/helmrelease.yaml")
	if !strings.Contains(hr, "kind: HelmRelease") {
		t.Error("helmrelease.yaml missing kind: HelmRelease")
	}
	oci := readDeployFile(t, dir, "deploy/k8s/oci-repository.yaml")
	if !strings.Contains(oci, "kind: OCIRepository") {
		t.Error("oci-repository.yaml missing kind: OCIRepository")
	}
	// No chart internals anywhere under deploy/.
	walkDeploy(t, dir, func(rel string) {
		if strings.Contains(rel, "Chart.yaml") || strings.Contains(rel, "deploy/helm/") ||
			strings.Contains(rel, "/templates/") {
			t.Errorf("editable chart leaked into the service repo: %q", rel)
		}
	})

	// The config env in the overlay traces to the service's prefix (#93, no drift).
	overlay := readDeployFile(t, dir, "deploy/k8s/values.yaml")
	if !strings.Contains(overlay, `envPrefix: "ORDERS_"`) {
		t.Errorf("k8s values overlay env prefix does not match the service config prefix:\n%s", overlay)
	}
}

// TestRenderDeploy_None confirms --deploy none emits no deploy artifacts.
func TestRenderDeploy_None(t *testing.T) {
	m, err := Options{
		Service: "orders", Resource: "Order", Backend: BackendGORM,
		Dir: t.TempDir(), Deploy: "none",
	}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(m.DeployTargets) != 0 {
		t.Fatalf("DeployTargets = %v, want empty for --deploy none", m.DeployTargets)
	}
	dir := t.TempDir()
	if err := renderDeploy(dir, m); err != nil {
		t.Fatalf("renderDeploy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy")); !os.IsNotExist(err) {
		t.Errorf("deploy/ should not exist for --deploy none")
	}
}

// TestValidate_UnknownDeployTarget rejects a bogus --deploy value.
func TestValidate_UnknownDeployTarget(t *testing.T) {
	_, err := Options{
		Service: "orders", Resource: "Order", Backend: BackendGORM,
		Dir: t.TempDir(), Deploy: "bogus",
	}.Validate()
	if err == nil {
		t.Fatal("Validate should reject an unknown deploy target")
	}
	if !strings.Contains(err.Error(), "unknown deploy target") {
		t.Errorf("error should name the unknown target, got: %v", err)
	}
}

// TestMainTemplate_GracefulShutdown is AC-5: both main entrypoints wire
// signal.NotifyContext (SIGTERM/Ctrl-C) so Serve returns on a signal and the
// graceful shutdown + OTel flush run. The previous fixed context.Background()
// never cancelled, so SIGTERM killed the process mid-request.
func TestMainTemplate_GracefulShutdown(t *testing.T) {
	for _, backend := range []Backend{BackendGORM, BackendEnt} {
		backend := backend
		t.Run(string(backend), func(t *testing.T) {
			m, err := Options{
				Service: "orders", Resource: "Order", Backend: backend, Dir: t.TempDir(),
			}.Validate()
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			src, err := renderTemplate(mainTemplate(backend), m)
			if err != nil {
				t.Fatalf("renderTemplate: %v", err)
			}
			s := string(src)

			if _, err := parser.ParseFile(token.NewFileSet(), "main.go", src, parser.AllErrors); err != nil {
				t.Fatalf("rendered main.go does not parse: %v\n%s", err, s)
			}
			for _, want := range []string{
				"signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)",
				`"os/signal"`,
				`"syscall"`,
				"defer stop()",
			} {
				if !strings.Contains(s, want) {
					t.Errorf("rendered main.go missing graceful-shutdown wiring %q", want)
				}
			}
			// The dead fixed-context pattern must be gone.
			if strings.Contains(s, "ctx := context.Background()") {
				t.Error("rendered main.go still uses a non-cancellable context.Background() for Serve (no graceful shutdown)")
			}
			// The host must run on the signal-derived ctx so cancellation drives the
			// servicekit.Run -> server.Serve graceful shutdown (WS-012: main hands ctx
			// to runHost, which threads it into servicekit.HostConfig.Context).
			if !strings.Contains(s, "runHost(ctx,") {
				t.Error("rendered main.go must call runHost(ctx, ...) so the signal cancellation drives shutdown")
			}
		})
	}
}

func readDeployFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func walkDeploy(t *testing.T, dir string, fn func(rel string)) {
	t.Helper()
	root := filepath.Join(dir, "deploy")
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		fn(rel)
		return nil
	})
}
