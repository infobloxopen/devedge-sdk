package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderInto renders the Phase-1 templates (incl. the image-publish workflow) for a
// gorm "orders" service into a temp dir and returns it. It exercises the real
// renderTemplates path (no apx/network), so the assertions below run against
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

// TestRenderImage_NoDockerfile guards the design choice: ko builds the image, so a
// generated service carries NO Dockerfile or .dockerignore.
func TestRenderImage_NoDockerfile(t *testing.T) {
	dir := renderInto(t, BackendGORM)
	for _, rel := range []string{"Dockerfile", ".dockerignore"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("%s should NOT be emitted (ko builds the image, no Dockerfile)", rel)
		}
	}
}

// TestRenderImage_MakeTarget asserts a new service's Makefile gains the local-build
// `make image` target, and that it auto-detects the docker socket (so `ko --local`
// works regardless of Docker Desktop / Rancher Desktop / colima / Linux).
func TestRenderImage_MakeTarget(t *testing.T) {
	dir := renderInto(t, BackendGORM)
	mk := readRendered(t, dir, "Makefile")
	for _, want := range []string{
		"\nimage:",               // a `make image` target
		"docker context inspect", // socket autodetect
		"DOCKER_HOST:-",          // respect an existing DOCKER_HOST, else autodetect
		"ko build --bare",        // ko build (ko.local => local load, no --local needed)
		"GOFLAGS=-trimpath",      // reproducible build
		"./cmd/orders",           // the service binary
	} {
		if !strings.Contains(mk, want) {
			t.Errorf("Makefile image target missing %q:\n%s", want, mk)
		}
	}
}

// TestRenderImage_Workflow asserts the publish workflow: triggers on merge to main
// + v* tags, has packages:write, generates before building, and builds a multi-arch
// distroless-static image with ko, pushed to a NON-NESTED repo-namespaced GHCR
// name. It also confirms the alternate-delim render left GitHub Actions ${{ }}
// intact and consumed every [[ ]] action.
func TestRenderImage_Workflow(t *testing.T) {
	dir := renderInto(t, BackendGORM)
	wf := readRendered(t, dir, ".github/workflows/image.yml")

	for _, want := range []string{
		`branches: ["main"]`, // publish on merge to main
		`tags: ["v*"]`,        // and on version tags
		"packages: write",     // GHCR push permission
		"run: make bootstrap", // generate gen/ before the ko build
		"GOFLAGS: -trimpath",  // reproducible build (no build-machine paths in the binary)
		"KO_DOCKER_REPO: ghcr.io/${{ github.repository }}${{ matrix.suffix }}",  // repo-namespaced
		"KO_DEFAULTBASEIMAGE: gcr.io/distroless/static-debian12:nonroot",        // distroless + static
		"go install github.com/google/ko@latest",                               // ko, no Dockerfile
		"ko build --bare",                                                       // bare => exactly KO_DOCKER_REPO
		"--platform=linux/amd64,linux/arm64",                                    // multi-arch
		"./cmd/orders",                                                          // build the service binary
		`binary: "orders"`,                                                      // matrix binary
		`ko login ghcr.io --username "${{ github.actor }}" --password "${{ secrets.GITHUB_TOKEN }}"`, // GHCR auth
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("image.yml missing %q:\n%s", want, wf)
		}
	}

	// Non-nested: the image name must be exactly ghcr.io/<owner>/<repo>[<suffix>] —
	// no extra "/<segment>" appended after the repository expression.
	if strings.Contains(wf, "ghcr.io/${{ github.repository }}/") {
		t.Errorf("image name is NESTED (a path segment follows the repo); want non-nested:\n%s", wf)
	}

	// No Docker build in the recipe (ko builds the image). "Dockerfile" may appear
	// in explanatory comments, so check the actual build commands/actions instead.
	for _, banned := range []string{"docker build", "buildx", "docker/build-push-action", "metadata-action"} {
		if strings.Contains(wf, banned) {
			t.Errorf("image.yml must not use %q (ko builds the image):\n%s", banned, wf)
		}
	}

	// The alternate-delim render must have consumed every [[ ]] action.
	if strings.Contains(wf, "[[") || strings.Contains(wf, "]]") {
		t.Errorf("image.yml still contains [[ ]] template delimiters (render gap):\n%s", wf)
	}
}

// TestRenderTemplateDelims_PassesThroughActionsSyntax is a focused unit check that
// the [[ ]] delimiter render evaluates our field substitution while leaving GitHub
// Actions ${{ }} verbatim.
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
}
