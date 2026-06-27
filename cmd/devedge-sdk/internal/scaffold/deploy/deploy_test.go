package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// sampleService is a representative service view: a postgres-backed "orders"
// service, matching what the scaffold projects from its Model.
func sampleService() ServiceView {
	return ServiceView{
		Name:      "orders",
		Module:    "github.com/acme/orders",
		EnvPrefix: "ORDERS_",
		GRPCPort:  "9090",
		HTTPPort:  "8080",
		Deps:      []Dependency{{Kind: "postgres", Image: "postgres:16-alpine"}},
	}
}

// TestSeam_RegistryHasBothFirstClassTargets is AC-1: the registry exposes the two
// real adapters (k8s + compose) plus the ECS stub — proving the seam is open for a
// third runtime with no core change (the stub satisfies the same interface).
func TestSeam_RegistryHasBothFirstClassTargets(t *testing.T) {
	for _, name := range []string{"k8s", "compose", "ecs"} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("target %q not registered", name)
		}
	}
	if got := DefaultTargets; len(got) != 2 || got[0] != "k8s" || got[1] != "compose" {
		t.Errorf("DefaultTargets = %v, want [k8s compose]", got)
	}
}

// TestRenderBothTargets_FileSet is AC-1/AC-2/AC-4: rendering the default targets
// emits the expected file set, the k8s output is Flux glue (HelmRelease +
// OCIRepository + values overlay) and carries NO editable chart, and every emitted
// YAML parses.
func TestRenderBothTargets_FileSet(t *testing.T) {
	arts, err := Render(DefaultTargets, sampleService(), Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := map[string][]byte{}
	for _, a := range arts {
		got[a.Path] = a.Contents
	}
	wantPaths := []string{
		"deploy/k8s/oci-repository.yaml",
		"deploy/k8s/helmrelease.yaml",
		"deploy/k8s/values.yaml",
		"deploy/k8s/README.md",
		"deploy/compose/docker-compose.yml",
	}
	var gotPaths []string
	for p := range got {
		gotPaths = append(gotPaths, p)
	}
	sort.Strings(gotPaths)
	sort.Strings(wantPaths)
	if strings.Join(gotPaths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("emitted paths = %v, want %v", gotPaths, wantPaths)
	}

	// AC-2: the k8s output is Flux-reconcilable glue, NOT a chart.
	hr := string(got["deploy/k8s/helmrelease.yaml"])
	if !strings.Contains(hr, "kind: HelmRelease") {
		t.Error("helmrelease.yaml missing kind: HelmRelease")
	}
	oci := string(got["deploy/k8s/oci-repository.yaml"])
	if !strings.Contains(oci, "kind: OCIRepository") {
		t.Error("oci-repository.yaml missing kind: OCIRepository")
	}
	if !strings.Contains(hr, "chart: devedge-service") {
		t.Error("HelmRelease must reference the framework chart by name")
	}

	// AC-2 failure-mode guard: NO editable chart leaks into the service repo.
	for p := range got {
		if strings.Contains(p, "Chart.yaml") || strings.Contains(p, "/templates/") || strings.Contains(p, "deploy/helm/") {
			t.Errorf("editable chart leaked into service repo: %q (devs must never author the chart)", p)
		}
	}

	// AC-2/AC-4: every emitted YAML file parses.
	for p, b := range got {
		if !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml") {
			continue
		}
		var v any
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Errorf("emitted %s does not parse as YAML: %v\n%s", p, err, b)
		}
	}
}

// TestK8sValuesOverlay_WiresFoundation cross-checks the values overlay against the
// known config surface (#93 env prefix) so it cannot drift from the code.
func TestK8sValuesOverlay_WiresFoundation(t *testing.T) {
	arts, err := Render([]string{"k8s"}, sampleService(), Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var overlay string
	for _, a := range arts {
		if a.Path == "deploy/k8s/values.yaml" {
			overlay = string(a.Contents)
		}
	}
	for _, want := range []string{
		`envPrefix: "ORDERS_"`,
		`grpcAddr: ":9090"`,
		`httpAddr: ":8080"`,
		"image:",
		"ghcr.io/acme/orders",
		"existingSecret:", // postgres dep => DSN via a pre-provisioned Secret
	} {
		if !strings.Contains(overlay, want) {
			t.Errorf("k8s values overlay missing %q:\n%s", want, overlay)
		}
	}
}

// TestComposeTarget_SameSurface is AC-4: the compose output wires the same
// config/health/observability surface, the postgres dep, and a grace period.
func TestComposeTarget_SameSurface(t *testing.T) {
	arts, err := Render([]string{"compose"}, sampleService(), Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	compose := string(arts[0].Contents)
	for _, want := range []string{
		"ORDERS_GRPC_ADDR:",
		"ORDERS_HTTP_ADDR:",
		"ORDERS_LOG_LEVEL:",
		"ORDERS_DSN:",
		"/healthz",                    // healthcheck hits the same liveness path
		"OTEL_EXPORTER_OTLP_ENDPOINT", // observability surface (commented activation)
		"stop_grace_period: 30s",
		"postgres:",
		"pg_isready",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("docker-compose.yml missing %q:\n%s", want, compose)
		}
	}
	var v any
	if err := yaml.Unmarshal(arts[0].Contents, &v); err != nil {
		t.Fatalf("docker-compose.yml does not parse: %v", err)
	}
}

// TestComposeTarget_NoPostgresWhenNotDeclared confirms the dep wiring is driven by
// the declared deps, not hard-coded: a service with no deps gets no DB service.
func TestComposeTarget_NoPostgresWhenNotDeclared(t *testing.T) {
	svc := sampleService()
	svc.Deps = nil
	arts, err := Render([]string{"compose"}, svc, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	compose := string(arts[0].Contents)
	if strings.Contains(compose, "pg_isready") || strings.Contains(compose, "POSTGRES_DB") {
		t.Errorf("compose rendered a postgres service for a depless service:\n%s", compose)
	}
}

// TestECSStub_SatisfiesSeamWithoutCoreChange is AC-1: the ECS stub compiles
// against the same Target interface (proving a third runtime needs only an
// adapter) and returns a clear not-implemented error rather than rendering.
func TestECSStub_SatisfiesSeamWithoutCoreChange(t *testing.T) {
	var _ Target = ecsTarget{} // compiles against the seam: no core change needed
	tgt, ok := Lookup("ecs")
	if !ok {
		t.Fatal("ecs stub not registered")
	}
	_, err := tgt.Render(sampleService(), Options{})
	if err == nil {
		t.Fatal("ecs stub Render must return a not-implemented error")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("ecs stub error should say not implemented, got: %v", err)
	}
}

// TestParseTargets covers the --deploy parsing: default, explicit, none, unknown.
func TestParseTargets(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", []string{"k8s", "compose"}, false},
		{"k8s,compose", []string{"k8s", "compose"}, false},
		{"compose", []string{"compose"}, false},
		{"none", nil, false},
		{"k8s,k8s", []string{"k8s"}, false}, // dedup
		{"bogus", nil, true},
	}
	for _, c := range cases {
		got, err := ParseTargets(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseTargets(%q): want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTargets(%q): %v", c.in, err)
			continue
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("ParseTargets(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestEmbeddedChart_LintAndTemplate is AC-2/AC-3: the framework-owned chart passes
// `helm lint` and `helm template` (when helm is on PATH) and the rendered
// Deployment wires the foundation — liveness /healthz, readiness /readyz, the
// config env (#93), OTEL_* (#90), the DSN Secret, ingress, resource limits, and
// terminationGracePeriodSeconds. When helm is absent, it still asserts every
// embedded chart file parses as YAML.
func TestEmbeddedChart_LintAndTemplate(t *testing.T) {
	files, err := ChartFiles()
	if err != nil {
		t.Fatalf("ChartFiles: %v", err)
	}
	if _, ok := files["Chart.yaml"]; !ok {
		t.Fatal("embedded chart missing Chart.yaml")
	}

	// Always: the values.yaml + Chart.yaml parse (the .tpl helper is not YAML).
	for p, b := range files {
		if !strings.HasSuffix(p, ".yaml") {
			continue
		}
		// templates/*.yaml are Helm templates (Go template syntax), not plain
		// YAML — skip those here; helm template (below) validates them.
		if strings.HasPrefix(p, "templates/") {
			continue
		}
		var v any
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Errorf("embedded chart %s does not parse as YAML: %v", p, err)
		}
	}

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH; skipped lint/template (embedded YAML parse already asserted)")
	}

	// Materialize the embedded chart to a temp dir for helm.
	dir := t.TempDir()
	for p, b := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	setArgs := []string{
		"--set", "image.repository=ghcr.io/acme/orders",
		"--set", "config.envPrefix=ORDERS_",
		"--set", "otel.exporter.otlp.endpoint=otel-collector:4317",
		"--set", "ingress.enabled=true",
		"--set", "ingress.host=orders.example.com",
		"--set", "dsn.value=postgres://x",
	}

	// helm lint
	lint := exec.Command(helm, append([]string{"lint", dir}, setArgs...)...)
	if out, err := lint.CombinedOutput(); err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}

	// helm template
	tmplArgs := append([]string{"template", "orders", dir}, setArgs...)
	out, err := exec.Command(helm, tmplArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	rendered := string(out)
	for _, want := range []string{
		"kind: Deployment",
		"kind: Service",
		"kind: Secret",
		"kind: Ingress",
		"path: /healthz",
		"path: /readyz",
		"name: ORDERS_GRPC_ADDR",
		"name: ORDERS_HTTP_ADDR",
		"name: ORDERS_LOG_LEVEL",
		"name: ORDERS_DSN",
		"secretKeyRef:",
		"name: OTEL_EXPORTER_OTLP_ENDPOINT",
		"terminationGracePeriodSeconds: 30",
		"cpu:",
		"memory:",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("helm template output missing %q", want)
		}
	}

	// AC-3 failure-mode guard: the rendered Deployment wires BOTH probes and the
	// liveness probe must not check deps (it is /healthz, not /readyz).
	if !strings.Contains(rendered, "livenessProbe:") || !strings.Contains(rendered, "readinessProbe:") {
		t.Error("rendered Deployment missing liveness/readiness probes")
	}
}

// TestEmittedChartRef_MatchesEmbeddedChart is the chart-drift guard (AC-2
// failure-mode): the chart name + version the emitted HelmRelease/OCIRepository
// pin MUST equal the embedded Chart.yaml's name + version. The embedded chart is
// the single source of truth and is what the framework publishes; if these drift,
// a scaffolded service references a chart coordinate that was never published.
func TestEmittedChartRef_MatchesEmbeddedChart(t *testing.T) {
	files, err := ChartFiles()
	if err != nil {
		t.Fatalf("ChartFiles: %v", err)
	}
	var chart struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(files["Chart.yaml"], &chart); err != nil {
		t.Fatalf("parse embedded Chart.yaml: %v", err)
	}

	// The exported constants the k8s target renders from must equal the embedded
	// chart (so a Chart.yaml bump that forgets the constants fails this test).
	if ChartName != chart.Name {
		t.Errorf("ChartName const %q != embedded Chart.yaml name %q", ChartName, chart.Name)
	}
	if DefaultChartVersion != chart.Version {
		t.Errorf("DefaultChartVersion const %q != embedded Chart.yaml version %q", DefaultChartVersion, chart.Version)
	}

	// And the emitted Flux artifacts must reference exactly that name + version.
	arts, err := Render([]string{"k8s"}, sampleService(), Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	emitted := map[string]string{}
	for _, a := range arts {
		emitted[a.Path] = string(a.Contents)
	}
	hr := emitted["deploy/k8s/helmrelease.yaml"]
	oci := emitted["deploy/k8s/oci-repository.yaml"]
	for _, want := range []string{
		"chart: " + chart.Name,             // HelmRelease chart.spec.chart
		`version: "` + chart.Version + `"`, // HelmRelease chart.spec.version
	} {
		if !strings.Contains(hr, want) {
			t.Errorf("HelmRelease missing %q (drift from embedded Chart.yaml):\n%s", want, hr)
		}
	}
	if !strings.Contains(oci, "/"+chart.Name) {
		t.Errorf("OCIRepository url does not reference the embedded chart name %q:\n%s", chart.Name, oci)
	}
	if !strings.Contains(oci, `tag: "`+chart.Version+`"`) {
		t.Errorf("OCIRepository tag does not pin the embedded chart version %q:\n%s", chart.Version, oci)
	}
}
