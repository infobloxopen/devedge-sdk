package scaffold

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// renderInto renders the Phase-1 templates (incl. the container image artifacts)
// for a gorm "orders" service into a temp dir and returns it. It exercises the
// real renderTemplates path (no apx/network), so the assertions below run against
// exactly what a scaffold emits.
func renderInto(t *testing.T, backend Backend) string {
	t.Helper()
	m, err := Options{
		Service:  "orders",
		Resource: "Order",
		Backend:  backend,
		Dir:      t.TempDir(),
	}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	dir := t.TempDir()
	if err := renderTemplates(dir, m); err != nil {
		t.Fatalf("renderTemplates: %v", err)
	}
	return dir
}

func readRendered(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestRenderImage_Dockerfile asserts the emitted Dockerfile is the distroless,
// static, multi-arch build the spec calls for, building ./cmd/<svc>.
func TestRenderImage_Dockerfile(t *testing.T) {
	for _, backend := range []Backend{BackendGORM, BackendEnt} {
		t.Run(string(backend), func(t *testing.T) {
			dir := renderInto(t, backend)
			df := readRendered(t, dir, "Dockerfile")
			for _, want := range []string{
				"gcr.io/distroless/static-debian12:nonroot", // distroless + static runtime base
				"CGO_ENABLED=0",                              // statically linked
				"--platform=$BUILDPLATFORM",                  // cross-compile, not QEMU
				"GOOS=${TARGETOS} GOARCH=${TARGETARCH}",      // honor buildx target arch
				"ARG BINARY=orders",                          // default to the service binary
				"-o /out/orders ./cmd/${BINARY}",             // build cmd/<svc> via BINARY
				`ENTRYPOINT ["/usr/local/bin/orders"]`,       // run the binary by default
				"USER nonroot:nonroot",                       // non-root
				"-X main.version=${VERSION}",                 // version stamp
			} {
				if !strings.Contains(df, want) {
					t.Errorf("Dockerfile missing %q:\n%s", want, df)
				}
			}
			// No CMD subcommand: the binary serves by default (matches compose, which
			// runs the image with no command).
			if strings.Contains(df, "\nCMD ") {
				t.Errorf("Dockerfile should not set a CMD (the binary serves by default):\n%s", df)
			}
		})
	}
}

// TestRenderImage_DockerignoreKeepsGen guards the one .dockerignore rule that
// would silently break the build: gen/ (the generated code the image compiles)
// must NOT be excluded from the build context.
func TestRenderImage_DockerignoreKeepsGen(t *testing.T) {
	dir := renderInto(t, BackendGORM)
	di := readRendered(t, dir, ".dockerignore")
	for _, line := range strings.Split(di, "\n") {
		switch strings.TrimSpace(line) {
		case "gen", "gen/", "/gen", "/gen/", "**/gen":
			t.Errorf(".dockerignore excludes generated code (%q) — the image build needs gen/:\n%s", line, di)
		}
	}
	if !strings.Contains(di, ".git") {
		t.Errorf(".dockerignore should drop .git from the build context:\n%s", di)
	}
}

// TestRenderImage_Workflow asserts the publish workflow: triggers on merge to main
// + v* tags, has packages:write, generates before building, pushes a multi-arch
// image to a NON-NESTED, repo-namespaced GHCR name, and that the alternate-delim
// render left GitHub Actions ${{ }} and metadata-action {{ }} intact.
func TestRenderImage_Workflow(t *testing.T) {
	dir := renderInto(t, BackendGORM)
	wf := readRendered(t, dir, ".github/workflows/image.yml")

	for _, want := range []string{
		`branches: ["main"]`,                               // publish on merge to main
		`tags: ["v*"]`,                                     // and on version tags
		"packages: write",                                  // GHCR push permission
		"run: make bootstrap",                              // generate gen/ before docker build
		"platforms: linux/amd64,linux/arm64",               // multi-arch
		"images: ghcr.io/${{ github.repository }}${{ matrix.suffix }}", // repo-namespaced
		`binary: "orders"`,                                 // matrix binary = service binary
		"BINARY=${{ matrix.binary }}",                      // passed to the Dockerfile
		"VERSION=${{ steps.meta.outputs.version }}",        // version stamp
		"type=semver,pattern={{version}}",                  // metadata-action {{ }} survived
		"enable={{is_default_branch}}",                     // metadata-action {{ }} survived
		"password: ${{ secrets.GITHUB_TOKEN }}",            // GHCR login
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("image.yml missing %q:\n%s", want, wf)
		}
	}

	// Non-nested: the image name must be exactly ghcr.io/<owner>/<repo>[<suffix>] —
	// there must be no extra "/<segment>" appended after the repository expression.
	if strings.Contains(wf, "ghcr.io/${{ github.repository }}/") {
		t.Errorf("image name is NESTED (a path segment follows the repo); want non-nested:\n%s", wf)
	}

	// The alternate-delim render must have consumed every [[ ]] action and left no
	// unrendered field reference behind.
	if strings.Contains(wf, "[[") || strings.Contains(wf, "]]") {
		t.Errorf("image.yml still contains [[ ]] template delimiters (render gap):\n%s", wf)
	}
}

// TestRenderImage_GoVersionNoDrift is the drift guard: the Dockerfile builder image
// (golang:X.Y) must track go.mod's `go X.Y` directive. If go.mod is bumped without
// the Dockerfile, this fails — the image would build the wrong toolchain.
func TestRenderImage_GoVersionNoDrift(t *testing.T) {
	dir := renderInto(t, BackendGORM)
	df := readRendered(t, dir, "Dockerfile")
	gomod := readRendered(t, dir, "go.mod")

	dfVer := regexp.MustCompile(`golang:(\d+\.\d+)`).FindStringSubmatch(df)
	if dfVer == nil {
		t.Fatalf("Dockerfile has no golang:X.Y builder tag:\n%s", df)
	}
	modVer := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(gomod)
	if modVer == nil {
		t.Fatalf("go.mod has no `go X.Y` directive:\n%s", gomod)
	}
	if dfVer[1] != modVer[1] {
		t.Errorf("Go version drift: Dockerfile golang:%s vs go.mod go %s — keep them in sync", dfVer[1], modVer[1])
	}
}

// TestRenderTemplateDelims_PassesThroughActionsSyntax is a focused unit check that
// the [[ ]] delimiter render evaluates our field substitution while leaving both
// ${{ }} (GitHub Actions) and {{ }} (metadata-action) verbatim.
func TestRenderTemplateDelims_PassesThroughActionsSyntax(t *testing.T) {
	m, err := Options{Service: "orders", Resource: "Order", Backend: BackendGORM, Dir: t.TempDir()}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := renderTemplateDelims("image.yml.tmpl", "[[", "]]", m)
	if err != nil {
		t.Fatalf("renderTemplateDelims: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `binary: "orders"`) {
		t.Errorf("[[ .BinName ]] was not evaluated:\n%s", s)
	}
	if !strings.Contains(s, "${{ github.repository }}") {
		t.Errorf("GitHub Actions ${{ }} did not pass through:\n%s", s)
	}
	if !strings.Contains(s, "{{version}}") {
		t.Errorf("metadata-action {{ }} did not pass through:\n%s", s)
	}
}
