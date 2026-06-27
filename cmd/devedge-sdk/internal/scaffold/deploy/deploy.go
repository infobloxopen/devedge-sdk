// Package deploy is the multi-runtime deployment-target seam (F038). A Target is
// an adapter that renders deploy artifacts for one runtime (Kubernetes/k3s via a
// framework-owned Helm chart + Flux, Docker Compose, and — as a documented stub
// — AWS ECS). The scaffold pipeline selects one or more targets by name and
// writes their Artifacts into the generated service repo.
//
// Adding a runtime is adding a Target adapter and registering it — no core change
// (the seam gate, AC-1). Everything here is TOOLING: it renders templates into
// the generated repo and pulls no dependency into the service runtime (AC-6).
package deploy

import (
	"fmt"
	"sort"
	"strings"
)

// Artifact is one rendered file destined for the generated service repo, at Path
// (relative to the repo root) with the given Contents.
type Artifact struct {
	Path     string
	Contents []byte
}

// Dependency is an infrastructure dependency the service declares (e.g. a
// Postgres database). Targets wire it into their runtime (a compose service, a
// chart value). Kept minimal — the seam only needs enough to render a default.
type Dependency struct {
	// Kind is the dependency type, e.g. "postgres".
	Kind string
	// Image is the container image used by runtimes that run the dep locally
	// (compose). Prod (k8s) references an external managed instance via the DSN.
	Image string
}

// ServiceView is the projection of the scaffold model that deploy targets
// consume. It is deliberately a small, stable struct (not the full scaffold
// Model) so the seam stays decoupled from scaffold internals: a target reads
// only what it needs to render. The config env names are NOT carried here —
// they are owned by config.ServerOptions and referenced symbolically by the
// chart/compose so they cannot drift (the EnvPrefix derives the lookup keys).
type ServiceView struct {
	// Name is the service/binary name, e.g. "orders".
	Name string
	// Module is the Go module path, e.g. "github.com/infobloxopen/orders".
	Module string
	// EnvPrefix is the config.Env prefix the service main loads with, e.g.
	// "ORDERS_". Combined with the config.ServerOptions keys it yields the exact
	// env var names the chart/compose set (e.g. ORDERS_GRPC_ADDR).
	EnvPrefix string
	// GRPCPort / HTTPPort are the default listen ports (numeric, no colon).
	GRPCPort string
	HTTPPort string
	// Deps are the infrastructure dependencies to wire into a runtime.
	Deps []Dependency
}

// Options are the per-render knobs shared by all targets. Concrete coordinates
// (the OCI registry host, the chart version) are placeholders here and surfaced
// as documented values/overlays — see the k8s target.
type Options struct {
	// ChartRepo is the OCI registry/host the published Helm chart lives under,
	// referenced by the emitted Flux OCIRepository source. A documented
	// placeholder the developer points at their registry.
	ChartRepo string
	// ChartVersion is the published chart version the HelmRelease pins.
	ChartVersion string
	// Namespace is the target k8s namespace for the HelmRelease + sources.
	Namespace string
	// GracePeriodSeconds is the shutdown grace window, paired with the scaffold's
	// signal.NotifyContext graceful shutdown (AC-5). Default 30.
	GracePeriodSeconds int
}

// withDefaults fills the documented placeholder defaults.
func (o Options) withDefaults(svc ServiceView) Options {
	if o.ChartRepo == "" {
		o.ChartRepo = "oci://ghcr.io/infobloxopen/charts"
	}
	if o.ChartVersion == "" {
		o.ChartVersion = "0.1.0"
	}
	if o.Namespace == "" {
		o.Namespace = svc.Name
	}
	if o.GracePeriodSeconds == 0 {
		o.GracePeriodSeconds = 30
	}
	return o
}

// Target renders deploy artifacts for one runtime. Implementations are
// registered with Register; adding one requires no core change (AC-1).
type Target interface {
	// Name is the target's registry key ("k8s", "compose", "ecs").
	Name() string
	// Render produces the artifacts for svc under opts. Paths are relative to the
	// generated service repo root.
	Render(svc ServiceView, opts Options) ([]Artifact, error)
}

var registry = map[string]Target{}

// Register adds a target to the registry. It panics on a duplicate name
// (a programming error in the init wiring).
func Register(t Target) {
	name := t.Name()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("deploy: target %q already registered", name))
	}
	registry[name] = t
}

// Lookup returns the registered target for name.
func Lookup(name string) (Target, bool) {
	t, ok := registry[name]
	return t, ok
}

// Names returns the registered target names, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DefaultTargets is the set rendered when --deploy is omitted: the two real,
// first-class runtimes. The ECS stub is opt-in (it errors on render).
var DefaultTargets = []string{"k8s", "compose"}

// ParseTargets resolves a comma-separated --deploy value into target names,
// validating each against the registry. An empty/whitespace value yields
// DefaultTargets. "none" disables deploy rendering (returns an empty slice).
func ParseTargets(csv string) ([]string, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return append([]string(nil), DefaultTargets...), nil
	}
	if csv == "none" {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(csv, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := Lookup(name); !ok {
			return nil, fmt.Errorf("unknown deploy target %q (known: %s)", name, strings.Join(Names(), ", "))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// Render runs the named targets and concatenates their artifacts. Target names
// are validated; an unknown name is an error. The returned artifacts are in
// target order, each target's order preserved.
func Render(names []string, svc ServiceView, opts Options) ([]Artifact, error) {
	opts = opts.withDefaults(svc)
	var arts []Artifact
	for _, name := range names {
		t, ok := Lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown deploy target %q", name)
		}
		got, err := t.Render(svc, opts)
		if err != nil {
			return nil, fmt.Errorf("deploy target %q: %w", name, err)
		}
		arts = append(arts, got...)
	}
	return arts, nil
}
