package scaffold

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/infobloxopen/devedge-sdk/cmd/devedge-sdk/internal/scaffold/deploy"
	"github.com/infobloxopen/devedge-sdk/slo"
)

// renderTemplate executes the named template (under templates/) against m.
func renderTemplate(name string, m *Model) ([]byte, error) {
	return renderTemplateDelims(name, "{{", "}}", m)
}

// renderTemplateDelims is renderTemplate with explicit action delimiters. The
// GitHub Actions workflow template (image.yml) is rendered with "[[" / "]]" so the
// workflow's own ${{ ... }} expressions and docker/metadata-action's {{ ... }}
// placeholders pass through verbatim instead of being parsed as Go template
// actions — only our [[ .Field ]] substitutions are evaluated.
func renderTemplateDelims(name, left, right string, m *Model) ([]byte, error) {
	t, err := template.New(name).Delims(left, right).Option("missingkey=error").ParseFS(templatesFS, "templates/"+name)
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, m); err != nil {
		return nil, fmt.Errorf("render template %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// writeFile writes content to dir/rel, creating parent directories.
func writeFile(dir, rel string, content []byte, perm os.FileMode) error {
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, content, perm)
}

// artifactWriter writes rendered artifacts and records what it wrote vs. skipped.
// With force=false it never clobbers an existing file (the retrofit path —
// `add deploy` — must not touch a service's hand-edited tree); with force=true it
// always overwrites (the new-service path, where the tree is freshly created).
type artifactWriter struct {
	force   bool
	Written []string
	Skipped []string
}

func (w *artifactWriter) write(dir, rel string, content []byte, perm os.FileMode) error {
	if !w.force {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			w.Skipped = append(w.Skipped, rel)
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := writeFile(dir, rel, content, perm); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	w.Written = append(w.Written, rel)
	return nil
}

// renderImageArtifacts renders the GHCR image-publish workflow (.github/workflows/
// image.yml) through w (so the retrofit can skip a file that already exists). The
// workflow builds the image with ko (no Dockerfile). It is rendered with "[[" /
// "]]" delimiters so the workflow's GitHub Actions ${{ ... }} expressions pass
// through verbatim. Shared by new-service rendering and the `add deploy` retrofit
// so both produce a BYTE-IDENTICAL workflow.
func renderImageArtifacts(dir string, m *Model, w *artifactWriter) error {
	content, err := renderTemplateDelims("image.yml.tmpl", "[[", "]]", m)
	if err != nil {
		return err
	}
	return w.write(dir, filepath.Join(".github", "workflows", "image.yml"), content, 0o644)
}

// appendMakefileImageTarget appends the `make image` target (a local ko build that
// auto-detects the docker socket) to dir/Makefile. It is idempotent: it does
// nothing if the Makefile already declares an `image:` target, and returns false if
// the target was already present or there is no Makefile to append to (a non-
// standard repo). Shared by new-service rendering and the `add deploy` retrofit so
// the local-build target is identical either way.
func appendMakefileImageTarget(dir string, m *Model) (bool, error) {
	path := filepath.Join(dir, "Makefile")
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	// A Makefile that reads the managed `de sync` shim already has an `image`
	// target (it delegates to `de image`), so appending a second local one would
	// collide. The new-service scaffold is exactly this case; retrofits onto a
	// non-devedge Makefile still get the local ko target below.
	if strings.Contains(string(existing), ".devedge/make/devedge.mk") {
		return false, nil
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.HasPrefix(line, "image:") {
			return false, nil // already has an image target
		}
	}
	frag, err := renderTemplate("image.mk.tmpl", m)
	if err != nil {
		return false, err
	}
	out := existing
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, frag...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, fmt.Errorf("append image target to Makefile: %w", err)
	}
	return true, nil
}

// renderTemplates renders the Phase-1 templates into dir (T-204). The proto file
// REPLACES the example proto apx init wrote; the rest are new files.
func renderTemplates(dir string, m *Model) error {
	type out struct {
		tmpl string // template name under templates/
		rel  string // destination path relative to dir
		perm os.FileMode
	}
	outs := []out{
		{"proto.proto.tmpl", filepath.Join("proto", m.ProtoPathSuffix, m.ProtoFile), 0o644},
		{"buf.yaml.tmpl", "buf.yaml", 0o644},
		{bufGenTemplate(m.Backend), "buf.gen.yaml", 0o644},
		{goModTemplate(m.Backend), "go.mod", 0o644},
		// WS-012 composable shape: the importable module/ unit + a thin cmd/<svc>
		// host (a standalone go run ./cmd/<svc> behaves exactly as before).
		{"module.go.tmpl", filepath.Join("module", "module.go"), 0o644},
		{"migrations.README.md.tmpl", filepath.Join("module", "migrations", "README.md"), 0o644},
		{mainTemplate(m.Backend), filepath.Join("cmd", m.BinName, "main.go"), 0o644},
		{"smoke_test.go.tmpl", filepath.Join("cmd", m.BinName, m.ServiceLower+"_smoke_test.go"), 0o644},
		// seccheck turns the SDK's security invariants into go test assertions so
		// "provable security in CI" is true for the shipped artifact out of the box
		// (BC-11): it runs under the scaffold's `make test` / `go test ./...` with
		// no extra harness.
		{"security_test.go.tmpl", filepath.Join("cmd", m.BinName, m.ServiceLower+"_security_test.go"), 0o644},
		{"Makefile.tmpl", "Makefile", 0o644},
		// The managed build shim `de sync` writes: generate/build/test/lint/image/
		// migrate-lint targets that delegate to `de`. Committed (like a lock file)
		// so a fresh scaffold builds via `make`/`de` immediately, before `de sync`
		// is ever run; it is byte-identical to `de sync` output, and `de sync`
		// regenerates it idempotently. The top-level Makefile `-include`s it.
		{"devedge.mk.tmpl", filepath.Join(".devedge", "make", "devedge.mk"), 0o644},
		{"README.md.tmpl", "README.md", 0o644},
		{"ci.yml.tmpl", filepath.Join(".github", "workflows", "ci.yml"), 0o644},
	}
	// The ent backend pins entc (entgo.io/ent/cmd/ent) via a build-tagged tools.go
	// so `go mod tidy` keeps the entc-only deps for `go generate ./gen/ent`.
	if m.Backend == BackendEnt {
		outs = append(outs, out{"tools.go.tmpl", "tools.go", 0o644})
	} else {
		// WS-012 composable seam: NewModule(db)/Models() let a composed host
		// (`de compose build`) build this module over one shared *gorm.DB without
		// naming its repository/model. gorm-only for now (the ent variant tracks
		// New<R>EntRepository + Schema.Create — a fast-follow).
		outs = append(outs, out{"module_compose.gorm.go.tmpl", filepath.Join("module", "compose.go"), 0o644})
	}
	for _, o := range outs {
		content, err := renderTemplate(o.tmpl, m)
		if err != nil {
			return err
		}
		if err := writeFile(dir, o.rel, content, o.perm); err != nil {
			return fmt.Errorf("write %s: %w", o.rel, err)
		}
	}
	// WS-025: a starter slo.yaml with the four grouped default SLOs derived from
	// the resource's standard AIP method names — a GOOD default reliability target
	// on disk day one (good/valid availability, mandatory error-budget policy stub,
	// 28d window, marked un-calibrated). `make slo` regenerates it from the real
	// OpenAPI (picking up custom methods).
	if err := renderSLO(dir, m); err != nil {
		return err
	}

	// Container image: the GHCR publish workflow (ko builds a distroless static
	// image — no Dockerfile). Emitted for every service regardless of --deploy (the
	// image is the unit of delivery; the k8s overlay + compose both reference it).
	// force=true: the new-service tree is freshly created, so always write.
	if err := renderImageArtifacts(dir, m, &artifactWriter{force: true}); err != nil {
		return err
	}
	if _, err := appendMakefileImageTarget(dir, m); err != nil {
		return err
	}
	if err := renderDeploy(dir, m, &artifactWriter{force: true}); err != nil {
		return err
	}
	return appendGitignore(dir, m)
}

// renderSLO writes a starter slo.yaml with the resource's grouped default SLOs
// (WS-025). It derives from the standard AIP method names, so it needs no
// generated OpenAPI — the service has a good default SLO before its first build.
func renderSLO(dir string, m *Model) error {
	plural := m.ResourcePlural
	if plural != "" {
		plural = strings.ToUpper(plural[:1]) + plural[1:]
	}
	// The day-one slo.yaml must be byte-identical to `de slo generate` (which reads
	// the enriched OpenAPI via slo.DefaultsFromOpenAPI). So the slug derives from
	// the gRPC service short name (ServiceType, e.g. "OrderService" -> slug
	// "order-service"), NOT ServiceLower, and the method set matches the scaffold's
	// proto template exactly: Create/Get/List/Update/Delete — no BatchGet, no
	// Undelete. (See templates/proto.proto.tmpl; if that gains RPCs, flip the flags
	// here and the parity test catches any drift.)
	doc, err := slo.DefaultsForResource(slo.ResourceDefaults{
		ServiceShort:    m.ServiceType,
		ServiceLabel:    m.ProtoPackage + "." + m.ServiceType,
		Resource:        m.Resource,
		ResourcePlural:  plural,
		IncludeBatchGet: false,
		SoftDelete:      false,
	}, slo.DefaultDeriveOptions())
	if err != nil {
		return fmt.Errorf("derive slo defaults: %w", err)
	}
	b, err := doc.Marshal()
	if err != nil {
		return fmt.Errorf("marshal slo.yaml: %w", err)
	}
	return writeFile(dir, "slo.yaml", b, 0o644)
}

// renderDeploy renders the selected deploy targets (F038) into the generated
// repo. Each target is an adapter behind the deploy seam; the service repo gets
// only the rendered artifacts (for k8s: a Flux HelmRelease + OCIRepository +
// values overlay — never the framework-owned chart, which stays embedded).
func renderDeploy(dir string, m *Model, w *artifactWriter) error {
	if len(m.DeployTargets) == 0 {
		return nil
	}
	arts, err := deploy.Render(m.DeployTargets, m.ServiceView(), deploy.Options{})
	if err != nil {
		return fmt.Errorf("render deploy: %w", err)
	}
	for _, a := range arts {
		if err := w.write(dir, a.Path, a.Contents, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func bufGenTemplate(b Backend) string {
	switch b {
	case BackendEnt:
		return "buf.gen.ent.yaml.tmpl"
	default:
		return "buf.gen.gorm.yaml.tmpl"
	}
}

func goModTemplate(b Backend) string {
	switch b {
	case BackendEnt:
		return "go.mod.ent.tmpl"
	default:
		return "go.mod.tmpl"
	}
}

// mainTemplate selects the hand-owned server entrypoint by backend: the gorm
// version opens a gorm.DB + AutoMigrate + New<R>Repository; the ent version opens
// an ent client + Schema.Create + the generated New<R>EntRepository (F027).
func mainTemplate(b Backend) string {
	switch b {
	case BackendEnt:
		return "main.ent.go.tmpl"
	default:
		return "main.go.tmpl"
	}
}

// appendGitignore appends the scaffold's ignore rules to the .gitignore apx wrote,
// after stripping the dead `/internal/gen/` rule apx emits by default — this
// scaffold's generated code lands in `/gen/` (our appended block), not
// `/internal/gen/`, so that line is misleading noise.
func appendGitignore(dir string, m *Model) error {
	content, err := renderTemplate("gitignore.append.tmpl", m)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ".gitignore")

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(existing) > 0 {
		filtered := stripGitignoreRule(string(existing), "/internal/gen/")
		if err := os.WriteFile(path, []byte(filtered), 0o644); err != nil {
			return err
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(content)
	return err
}

// stripGitignoreRule removes the line exactly matching rule (and an immediately
// preceding "# Generated code" comment that becomes orphaned) from a .gitignore.
func stripGitignoreRule(content, rule string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == rule {
			// Drop a now-orphaned "# Generated code" header sitting just above it.
			if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "# Generated code" {
				out = out[:n-1]
			}
			continue
		}
		out = append(out, ln)
	}
	// Drop leading blank lines left behind when the stripped rule + its header were
	// at the top of the file.
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	return strings.Join(out, "\n")
}

// vendorMirrors copies the embedded infoblox annotation mirrors into
// dir/proto/infoblox, pinned to the SDK version (D-4; T-205).
func vendorMirrors(dir string) error {
	return fs.WalkDir(mirrorsFS, "mirrors", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := mirrorsFS.ReadFile(p)
		if err != nil {
			return err
		}
		// strip the leading "mirrors/" so files land at proto/infoblox/...
		rel := filepath.Join("proto", p[len("mirrors/"):])
		return writeFile(dir, rel, data, 0o644)
	})
}

// MirrorFiles returns the embedded mirror paths and contents (for the drift test).
func MirrorFiles() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(mirrorsFS, "mirrors", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := mirrorsFS.ReadFile(p)
		if err != nil {
			return err
		}
		out[p[len("mirrors/"):]] = data
		return nil
	})
	return out, err
}
