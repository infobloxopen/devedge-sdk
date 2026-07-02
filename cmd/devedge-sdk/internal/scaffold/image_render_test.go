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

// TestRenderImage_MakeTarget asserts a new service's `make image` comes from the
// managed `de sync` fragment and delegates to `de image` (the pinned-ko builder,
// which auto-detects the docker socket) — NOT an inlined local ko build appended
// to the top Makefile (that append is skipped once the fragment is present).
func TestRenderImage_MakeTarget(t *testing.T) {
	dir := renderInto(t, BackendGORM)
	mk := readRendered(t, dir, "Makefile")
	if !strings.Contains(mk, "-include .devedge/make/devedge.mk") {
		t.Errorf("Makefile must read the managed fragment (which provides `image`):\n%s", mk)
	}
	// The top-level Makefile must NOT inline a second local ko build (would collide
	// with the fragment's `image` target).
	for _, banned := range []string{"docker context inspect", "ko build --bare"} {
		if strings.Contains(mk, banned) {
			t.Errorf("top-level Makefile must not inline a local ko build (%q); `image` comes from the fragment -> `de image`:\n%s", banned, mk)
		}
	}
	// The managed fragment owns `image`, delegating to `de image`.
	frag := readRendered(t, dir, ".devedge/make/devedge.mk")
	if !strings.Contains(frag, "image:    ; @de image") {
		t.Errorf("managed fragment must provide `image: @de image`:\n%s", frag)
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
		"run: de generate",    // regenerate gen/ before the image build
		"go install github.com/infobloxopen/devedge/cmd/de@",                   // pinned de (the build authority) — no @latest
		"KO_DOCKER_REPO: ghcr.io/${{ github.repository }}${{ matrix.suffix }}", // repo-namespaced
		"de image --push --repo",                                               // build+push via de image (pinned ko)
		"--base gcr.io/distroless/static-debian12:nonroot",                     // distroless + static base
		"--bare",                                                               // bare => exactly KO_DOCKER_REPO
		"./cmd/orders",                                                         // build the service binary
		`binary: "orders"`,                                                     // matrix binary
		"docker/login-action@v3",                                               // GHCR auth (ko reads the docker config)
		"registry: ghcr.io",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("image.yml missing %q:\n%s", want, wf)
		}
	}
	// No @latest tool installs in the converted workflow (hermetic: pinned de).
	if strings.Contains(wf, "@latest") {
		t.Errorf("image.yml must not install any tool at @latest:\n%s", wf)
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
