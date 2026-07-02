package scaffold

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/infobloxopen/devedge-sdk/cmd/devedge-sdk/internal/scaffold/deploy"
)

// AddDeployOptions configures `add deploy` — the retrofit that adds the container
// image + deploy artifacts to an EXISTING devedge-sdk service (one scaffolded by
// `new service`). The service name, module path, and backend are detected from the
// repo; ports default to the scaffold's unless overridden.
type AddDeployOptions struct {
	Dir       string // existing service repo root (default ".")
	Name      string // binary/service name; auto-detected from cmd/<name> when empty
	GRPCPort  string // default "9090" (the scaffold default)
	HTTPPort  string // default "8080" (the scaffold default)
	Deploy    string // deploy targets csv (k8s,compose); "none" to skip; empty = both
	ImageOnly bool   // render only Dockerfile/.dockerignore/image.yml (skip deploy/)
	Force     bool   // overwrite files that already exist (default: skip them)
}

// AddDeployResult reports the resolved service identity and what was written vs.
// skipped (skipped = already present and --force not set).
type AddDeployResult struct {
	Service string // PascalCase service name, e.g. "Orders"
	Bin     string // binary/cmd directory name, e.g. "orders"
	Module  string
	Backend Backend
	Written []string
	Skipped []string
}

// AddDeploy renders the container-image + deploy artifacts into an EXISTING
// devedge-sdk service at opts.Dir — BYTE-IDENTICAL to what `new service` emits for
// the same service (same templates, same model fields). It never overwrites an
// existing file unless opts.Force, so a service's hand-edited tree is safe; the
// result lists what was written vs. skipped.
func AddDeploy(opts AddDeployOptions, out io.Writer) (*AddDeployResult, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = "."
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("directory %q not found", dir)
	}

	module, err := moduleFromGoMod(dir)
	if err != nil {
		return nil, err
	}
	backend := backendFromGoMod(dir)
	bin, err := detectBinary(dir, opts.Name)
	if err != nil {
		return nil, err
	}

	deployTargets, err := deploy.ParseTargets(opts.Deploy)
	if err != nil {
		return nil, err
	}
	if opts.ImageOnly {
		deployTargets = nil
	}

	m := modelForExisting(module, bin, defaultIfEmpty(opts.GRPCPort, "9090"), defaultIfEmpty(opts.HTTPPort, "8080"), backend, deployTargets)

	fmt.Fprintf(out, "• retrofitting %s (module %s, backend %s, binary %s)\n", m.Service, module, backend, bin)
	w := &artifactWriter{force: opts.Force}
	if err := renderImageArtifacts(dir, m, w); err != nil {
		return nil, err
	}
	if err := renderDeploy(dir, m, w); err != nil {
		return nil, err
	}
	// Add the local-build target (`make image`) to the existing Makefile so a
	// retrofitted service gets the same local DX as a new one. Idempotent — it is a
	// no-op when the Makefile already has an `image:` target or there is none.
	written, skipped := w.Written, w.Skipped
	if appended, err := appendMakefileImageTarget(dir, m); err != nil {
		return nil, err
	} else if appended {
		written = append(written, "Makefile (+image target)")
	}

	return &AddDeployResult{
		Service: m.Service,
		Bin:     bin,
		Module:  module,
		Backend: backend,
		Written: written,
		Skipped: skipped,
	}, nil
}

// moduleFromGoMod reads the module path from dir/go.mod.
func moduleFromGoMod(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod (is %q a Go module root?): %w", dir, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			if mod := strings.Trim(strings.TrimSpace(rest), `"`); mod != "" {
				return mod, nil
			}
		}
	}
	return "", fmt.Errorf("no `module` directive in %s", filepath.Join(dir, "go.mod"))
}

// backendFromGoMod infers the persistence backend from the consumer go.mod: an ent
// service requires entgo.io/ent; a gorm service requires gorm.io/gorm. Defaults to
// gorm (the scaffold default) when neither is present.
func backendFromGoMod(dir string) Backend {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err == nil && strings.Contains(string(data), "entgo.io/ent") {
		return BackendEnt
	}
	return BackendGORM
}

// detectBinary resolves the service binary name. An explicit override wins;
// otherwise there must be exactly one cmd/<name> directory.
func detectBinary(dir, override string) (string, error) {
	if o := strings.TrimSpace(override); o != "" {
		return o, nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, "cmd"))
	if err != nil {
		return "", fmt.Errorf("no cmd/ directory in %q — pass --name to set the binary", dir)
	}
	var bins []string
	for _, e := range entries {
		if e.IsDir() {
			bins = append(bins, e.Name())
		}
	}
	switch len(bins) {
	case 0:
		return "", fmt.Errorf("no cmd/<name> binary directory found in %q — pass --name", dir)
	case 1:
		return bins[0], nil
	default:
		return "", fmt.Errorf("multiple binaries under cmd/ (%s) — pass --name to choose one", strings.Join(bins, ", "))
	}
}

// modelForExisting builds the minimal Model the image + deploy templates need for a
// retrofit. Only the fields those templates (and Model.ServiceView) reference are
// populated; the proto/resource codegen fields are unused by `add deploy`. The
// populated fields are derived exactly as Options.Validate derives them for a new
// service, so the rendered artifacts are byte-identical.
func modelForExisting(module, bin, grpcPort, httpPort string, backend Backend, deployTargets []string) *Model {
	return &Model{
		Service:       pascal(bin),
		ServiceLower:  bin,
		BinName:       bin,
		ServiceUpper:  strings.ToUpper(bin),
		Module:        module,
		RepoName:      module[strings.LastIndex(module, "/")+1:],
		Backend:       backend,
		GRPCPort:      grpcPort,
		HTTPPort:      httpPort,
		DeVersion:     deInstallVersion,
		DeployTargets: deployTargets,
	}
}

func defaultIfEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
