package deploy

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"text/template"
)

func init() { Register(k8sTarget{}) }

// k8sTarget is the first-class Kubernetes/k3s adapter. It emits a Flux-reconcilable
// trio into the service repo's deploy/k8s/ — a HelmRelease, an OCIRepository
// source pointing at the published framework chart, and a thin values overlay —
// and NEVER the chart internals (AC-2: devs never author the chart).
type k8sTarget struct{}

func (k8sTarget) Name() string { return "k8s" }

func (k8sTarget) Render(svc ServiceView, opts Options) ([]Artifact, error) {
	opts = opts.withDefaults(svc)
	data := k8sData{
		Service:            svc.Name,
		Namespace:          opts.Namespace,
		ChartRepo:          opts.ChartRepo,
		ChartName:          "devedge-service",
		ChartVersion:       opts.ChartVersion,
		EnvPrefix:          svc.EnvPrefix,
		GRPCPort:           svc.GRPCPort,
		HTTPPort:           svc.HTTPPort,
		GRPCAddr:           ":" + svc.GRPCPort,
		HTTPAddr:           ":" + svc.HTTPPort,
		GracePeriodSeconds: opts.GracePeriodSeconds,
		ImageRepo:          imageRepoFromModule(svc.Module, svc.Name),
		HasPostgres:        hasDep(svc, "postgres"),
	}
	var arts []Artifact
	for _, f := range []struct{ name, tmpl string }{
		{"deploy/k8s/oci-repository.yaml", k8sOCIRepositoryTmpl},
		{"deploy/k8s/helmrelease.yaml", k8sHelmReleaseTmpl},
		{"deploy/k8s/values.yaml", k8sValuesOverlayTmpl},
		{"deploy/k8s/README.md", k8sReadmeTmpl},
	} {
		b, err := renderText(f.name, f.tmpl, data)
		if err != nil {
			return nil, err
		}
		arts = append(arts, Artifact{Path: f.name, Contents: b})
	}
	return arts, nil
}

type k8sData struct {
	Service            string
	Namespace          string
	ChartRepo          string
	ChartName          string
	ChartVersion       string
	EnvPrefix          string
	GRPCPort           string
	HTTPPort           string
	GRPCAddr           string
	HTTPAddr           string
	GracePeriodSeconds int
	ImageRepo          string
	HasPostgres        bool
}

// imageRepoFromModule derives a sensible default image repository from the Go
// module path (a documented placeholder the developer overrides). e.g.
// github.com/acme/orders -> ghcr.io/acme/orders.
func imageRepoFromModule(module, svc string) string {
	m := strings.TrimPrefix(module, "github.com/")
	if m == "" || m == module {
		// non-github module or empty: fall back to the service name.
		return "ghcr.io/CHANGEME/" + svc
	}
	return "ghcr.io/" + m
}

func hasDep(svc ServiceView, kind string) bool {
	for _, d := range svc.Deps {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

func renderText(name, tmpl string, data any) ([]byte, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// ChartFiles returns the embedded framework chart as path->contents, with the
// leading "helm/chart/" stripped (so paths are chart-root-relative). Used by the
// chart lint/template test and, in a release step, the chart publish to OCI.
func ChartFiles() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(chartFS, "helm/chart", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := chartFS.ReadFile(p)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(p, "helm/chart/")] = b
		return nil
	})
	return out, err
}

// ChartName is the framework chart's name (Chart.yaml `name`), referenced by the
// emitted HelmRelease chart.spec.chart.
const ChartName = "devedge-service"

var _ = path.Base // keep path imported for future use without churn

const k8sOCIRepositoryTmpl = `# Flux source for the framework-owned Helm chart. The chart is PUBLISHED by the
# framework to an OCI registry (see deploy/k8s/README.md); this service repo never
# carries the chart itself — only this reference + the HelmRelease + values
# overlay. Point .spec.url at your published chart registry.
apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: OCIRepository
metadata:
  name: {{.Service}}-chart
  namespace: {{.Namespace}}
spec:
  interval: 10m
  # CHANGEME: the OCI registry the framework publishes the chart to.
  url: {{.ChartRepo}}/{{.ChartName}}
  ref:
    tag: "{{.ChartVersion}}"
`

const k8sHelmReleaseTmpl = `# Flux HelmRelease: reconciles the framework chart (via the OCIRepository source)
# with this service's values overlay. This is the ONLY deploy artifact you edit —
# through deploy/k8s/values.yaml. The chart internals are framework-owned.
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: {{.Service}}
  namespace: {{.Namespace}}
spec:
  interval: 10m
  chart:
    spec:
      chart: {{.ChartName}}
      version: "{{.ChartVersion}}"
      sourceRef:
        kind: OCIRepository
        name: {{.Service}}-chart
        namespace: {{.Namespace}}
  # Values overlay lives beside this file so it stays reviewable in git; Flux
  # merges deploy/k8s/values.yaml at apply time.
  valuesFrom:
    - kind: ConfigMap
      name: {{.Service}}-values
      valuesKey: values.yaml
`

const k8sValuesOverlayTmpl = `# Thin values overlay for {{.Service}} — the ONLY chart input you author. Wraps
# the framework chart (deploy/k8s/helmrelease.yaml) with this service's specifics.
# Apply it as a ConfigMap named {{.Service}}-values (valuesKey values.yaml) that
# the HelmRelease references, e.g.:
#   kubectl create configmap {{.Service}}-values -n {{.Namespace}} \
#     --from-file=values.yaml=deploy/k8s/values.yaml --dry-run=client -o yaml | kubectl apply -f -
image:
  repository: {{.ImageRepo}}
  tag: ""   # defaults to the chart appVersion; set to your release tag

config:
  # The config.ServerOptions env prefix this service loads with (config.Env).
  envPrefix: "{{.EnvPrefix}}"
  grpcAddr: "{{.GRPCAddr}}"
  httpAddr: "{{.HTTPAddr}}"
  logLevel: info

service:
  grpcPort: {{.GRPCPort}}
  httpPort: {{.HTTPPort}}

otel:
  exporter:
    otlp:
      # Point at your collector to activate tracing/metrics export (#90). Empty
      # no-ops cleanly.
      endpoint: ""   # e.g. otel-collector.observability:4317
      protocol: grpc

dsn:
{{- if .HasPostgres}}
  # Postgres-backed: provision the DSN out of band (Vault/ExternalSecrets) and
  # reference it here so the connection string never lands in git.
  existingSecret: "{{.Service}}-dsn"
  secretKey: dsn
{{- else}}
  # Leave value empty to use the service's built-in default; in prod set
  # existingSecret to a pre-provisioned Secret.
  value: ""
  existingSecret: ""
  secretKey: dsn
{{- end}}

ingress:
  enabled: false
  # host: {{.Service}}.example.com

# Graceful shutdown window — keep paired with the service's signal.NotifyContext
# shutdown and the chart default.
terminationGracePeriodSeconds: {{.GracePeriodSeconds}}
`

const k8sReadmeTmpl = `# Kubernetes / k3s deploy (Flux GitOps)

This service deploys via a **framework-owned Helm chart you never author**. This
directory carries only the GitOps glue:

| File | Purpose |
|------|---------|
| ` + "`oci-repository.yaml`" + ` | Flux ` + "`OCIRepository`" + ` source — where the published chart lives. |
| ` + "`helmrelease.yaml`" + ` | Flux ` + "`HelmRelease`" + ` — reconciles the chart with your overlay. |
| ` + "`values.yaml`" + ` | The **thin overlay** — the only chart input you edit. |

The chart itself (Deployment with liveness ` + "`/healthz`" + ` + readiness ` + "`/readyz`" + `,
the config env, ` + "`OTEL_*`" + ` export, the DSN Secret, ingress, resource limits, and
` + "`terminationGracePeriodSeconds`" + `) lives in the SDK and is **published by the
framework** to an OCI registry.

## Wire it up

1. Edit ` + "`values.yaml`" + `: set ` + "`image.repository`" + ` (and ` + "`image.tag`" + ` for a pinned
   release), the OTEL collector endpoint, and the DSN.
2. Point ` + "`oci-repository.yaml`" + ` ` + "`spec.url`" + ` at your published chart registry
   (default ` + "`{{.ChartRepo}}/{{.ChartName}}`" + `).
3. Apply the overlay as a ConfigMap the HelmRelease references:
   ` + "```sh" + `
   kubectl create configmap {{.Service}}-values -n {{.Namespace}} \
     --from-file=values.yaml=deploy/k8s/values.yaml \
     --dry-run=client -o yaml | kubectl apply -f -
   ` + "```" + `
4. Commit ` + "`deploy/k8s/`" + ` and let Flux reconcile it.

## Dev ↔ prod coherence

The SAME chart backs local dev (` + "`de project up --deploy`" + ` renders it via
` + "`helm template`" + `) and prod (Flux reconciles the published chart). One chart, two
reconcilers — they cannot drift.
`
