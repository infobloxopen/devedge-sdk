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
		{"Makefile.tmpl", "Makefile", 0o644},
		{"README.md.tmpl", "README.md", 0o644},
		{"ci.yml.tmpl", filepath.Join(".github", "workflows", "ci.yml"), 0o644},
		// Container image: a distroless, static build + the GHCR publish workflow.
		// Emitted for every service regardless of --deploy (the image is the unit
		// of delivery; the k8s overlay references it and compose builds it).
		{"Dockerfile.tmpl", "Dockerfile", 0o644},
		{"dockerignore.tmpl", ".dockerignore", 0o644},
	}
	// The ent backend pins entc (entgo.io/ent/cmd/ent) via a build-tagged tools.go
	// so `go mod tidy` keeps the entc-only deps for `go generate ./gen/ent`.
	if m.Backend == BackendEnt {
		outs = append(outs, out{"tools.go.tmpl", "tools.go", 0o644})
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
	// The GHCR image-publish workflow is rendered with "[[" / "]]" delimiters so the
	// workflow's GitHub Actions ${{ ... }} and docker/metadata-action {{ ... }} pass
	// through untouched (see renderTemplateDelims).
	imageWorkflow, err := renderTemplateDelims("image.yml.tmpl", "[[", "]]", m)
	if err != nil {
		return err
	}
	if err := writeFile(dir, filepath.Join(".github", "workflows", "image.yml"), imageWorkflow, 0o644); err != nil {
		return fmt.Errorf("write .github/workflows/image.yml: %w", err)
	}
	if err := renderDeploy(dir, m); err != nil {
		return err
	}
	return appendGitignore(dir, m)
}

// renderDeploy renders the selected deploy targets (F038) into the generated
// repo. Each target is an adapter behind the deploy seam; the service repo gets
// only the rendered artifacts (for k8s: a Flux HelmRelease + OCIRepository +
// values overlay — never the framework-owned chart, which stays embedded).
func renderDeploy(dir string, m *Model) error {
	if len(m.DeployTargets) == 0 {
		return nil
	}
	arts, err := deploy.Render(m.DeployTargets, m.ServiceView(), deploy.Options{})
	if err != nil {
		return fmt.Errorf("render deploy: %w", err)
	}
	for _, a := range arts {
		if err := writeFile(dir, a.Path, a.Contents, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", a.Path, err)
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
