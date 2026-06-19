package scaffold

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"text/template"
)

// renderTemplate executes the named template (under templates/) against m.
func renderTemplate(name string, m *Model) ([]byte, error) {
	t, err := template.New(name).Option("missingkey=error").ParseFS(templatesFS, "templates/"+name)
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
		{"go.mod.tmpl", "go.mod", 0o644},
		{"main.go.tmpl", filepath.Join("server", "main.go"), 0o644},
		{"smoke_test.go.tmpl", filepath.Join("server", m.ServiceLower+"_smoke_test.go"), 0o644},
		{"Makefile.tmpl", "Makefile", 0o644},
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
	return appendGitignore(dir, m)
}

func bufGenTemplate(b Backend) string {
	switch b {
	case BackendEnt:
		return "buf.gen.ent.yaml.tmpl"
	default:
		return "buf.gen.gorm.yaml.tmpl"
	}
}

// appendGitignore appends the scaffold's ignore rules to the .gitignore apx wrote.
func appendGitignore(dir string, m *Model) error {
	content, err := renderTemplate("gitignore.append.tmpl", m)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ".gitignore")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(content)
	return err
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
