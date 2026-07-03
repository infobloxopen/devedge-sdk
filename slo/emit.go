package slo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/template"
)

// Emit targets for Render.
const (
	TargetPrometheus = "prometheus"
	TargetGrafana    = "grafana"
	TargetLoki       = "loki"
)

// windowToken is the placeholder a raw-query metric source may use for the rate
// window; emitters replace it with each burn-rate window (5m, 1h, ...) when
// generating the per-window recording rules. A raw query with no token is used
// verbatim at every window.
const windowToken = "$window"

// RenderOptions tune projection.
type RenderOptions struct {
	// Naming maps OTel signals to the Prometheus metric/label names. Zero value
	// uses DefaultGRPCNaming.
	Naming MetricNaming
	// PresetDir, when non-empty and containing "<target>.tmpl", renders from that
	// Go text/template (executed with the *Document) instead of the built-in
	// emitter. This is the seam the internal Grafana-Operator overlay uses
	// (de slo render --preset-dir). A missing preset falls back to the built-in.
	PresetDir string
}

// naming returns the effective MetricNaming (default gRPC when unset). A
// MetricNaming with no status label is treated as unset.
func (o RenderOptions) naming() MetricNaming {
	if o.Naming.StatusLabel == "" {
		return DefaultGRPCNaming()
	}
	return o.Naming
}

// Rendered is one emitted artifact: a suggested filename and its bytes.
type Rendered struct {
	Filename string
	Content  []byte
}

// Render projects an OpenSLO Document to a backend target. When opts.PresetDir
// holds "<target>.tmpl", it renders from that template; otherwise it uses the
// built-in open-core emitter. Unknown targets are an error (fail-loud).
func Render(target string, doc *Document, opts RenderOptions) ([]Rendered, error) {
	if opts.PresetDir != "" {
		tmplPath := filepath.Join(opts.PresetDir, target+".tmpl")
		if _, err := os.Stat(tmplPath); err == nil {
			return renderPreset(target, tmplPath, doc)
		}
	}
	switch target {
	case TargetPrometheus:
		return emitPrometheus(doc, opts.naming())
	case TargetGrafana:
		return emitGrafana(doc, opts.naming())
	case TargetLoki:
		return emitLoki(doc, opts.naming())
	default:
		return nil, fmt.Errorf("slo: unknown render target %q (want prometheus|grafana|loki)", target)
	}
}

// renderPreset executes a preset template with the Document as data.
func renderPreset(target, tmplPath string, doc *Document) ([]Rendered, error) {
	b, err := os.ReadFile(tmplPath)
	if err != nil {
		return nil, fmt.Errorf("slo: read preset %s: %w", tmplPath, err)
	}
	t, err := template.New(target).Funcs(presetFuncs()).Parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("slo: parse preset %s: %w", tmplPath, err)
	}
	var out []byte
	buf := &writerTo{}
	if err := t.Execute(buf, doc); err != nil {
		return nil, fmt.Errorf("slo: render preset %s: %w", tmplPath, err)
	}
	out = buf.b
	return []Rendered{{Filename: target + "-preset.yaml", Content: out}}, nil
}

// presetFuncs exposes a few helpers to preset templates (the internal overlay
// wraps the same artifacts, so it needs the built-in emitters too).
func presetFuncs() template.FuncMap {
	return template.FuncMap{
		"prometheusRule": func(doc *Document) (string, error) {
			rs, err := emitPrometheus(doc, DefaultGRPCNaming())
			if err != nil || len(rs) == 0 {
				return "", err
			}
			return string(rs[0].Content), nil
		},
		"grafanaDashboards": func(doc *Document) ([]Rendered, error) {
			return emitGrafana(doc, DefaultGRPCNaming())
		},
	}
}

type writerTo struct{ b []byte }

func (w *writerTo) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }

// formatFloat renders a float without trailing zeros or exponent, so generated
// query strings and thresholds are stable and readable ("0.25", "14.4", "28").
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
