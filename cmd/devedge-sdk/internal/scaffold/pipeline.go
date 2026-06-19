package scaffold

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Generate runs the full scaffold pipeline for opts (D-1..D-5):
//
//	preflight → apx init app → render templates → vendor mirrors →
//	[buf generate + go mod tidy]
//
// out receives human-readable progress. It never leaves a half-written tree on a
// preflight failure; once apx init has run it writes into the target dir.
func Generate(ctx context.Context, opts Options, out io.Writer) (*Model, error) {
	m, err := opts.Validate()
	if err != nil {
		return nil, err
	}

	if err := preflight(ctx, opts.NoGenerate); err != nil {
		return nil, err
	}

	if err := prepareDir(opts.Dir, opts.Force); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(out, "• apx init app proto/%s (org=%s repo=%s)\n", m.ProtoPathSuffix, m.Org, m.RepoName)
	if err := apxInitApp(ctx, abs, m); err != nil {
		return nil, fmt.Errorf("apx init app: %w", err)
	}

	fmt.Fprintf(out, "• rendering templates (backend=%s)\n", m.Backend)
	if err := renderTemplates(abs, m); err != nil {
		return nil, err
	}

	fmt.Fprintf(out, "• vendoring infoblox annotation mirrors (SDK %s)\n", m.SDKVersion)
	if err := vendorMirrors(abs); err != nil {
		return nil, fmt.Errorf("vendor mirrors: %w", err)
	}

	if err := reconcileApxModuleRoots(abs); err != nil {
		return nil, fmt.Errorf("reconcile apx.yaml: %w", err)
	}

	// Confirm apx is happy with the assembled config (AC-006).
	fmt.Fprintf(out, "• apx config validate\n")
	if err := runCmd(ctx, abs, nil, "apx", "config", "validate"); err != nil {
		return nil, fmt.Errorf("apx config validate failed: %w", err)
	}

	if opts.NoGenerate {
		fmt.Fprintf(out, "• --no-generate: skipping buf generate + go mod tidy\n")
		return m, nil
	}

	fmt.Fprintf(out, "• buf generate\n")
	if err := runBufGenerate(ctx, abs); err != nil {
		return nil, fmt.Errorf("buf generate: %w", err)
	}

	if m.Backend == BackendEnt {
		// ent is a TWO-STEP generate, and the steps have an ordering hazard: buf
		// ran protoc-gen-ent (the ent SCHEMAS + the New<R>EntRepository adapter +
		// main.go), all of which import the ent CLIENT packages (gen/ent/order,
		// .../predicate, .../runtime) that entc has NOT generated yet. A plain
		// `go mod tidy` here would try to resolve those not-yet-existing internal
		// packages as remote modules and fail. So:
		//   1. `go mod tidy -e` (best-effort: ignore the unresolvable internal
		//      client-package errors) just to pull the entc toolchain deps into
		//      go.sum so entc can run;
		//   2. `go generate ./gen/ent` → entc turns the schemas into the client;
		//   3. a clean `go mod tidy` now that every package exists.
		fmt.Fprintf(out, "• go mod tidy -e (seed entc deps)\n")
		// Errors expected/ignored: the client packages don't exist until step 2.
		_ = runCmd(ctx, abs, nil, "go", "mod", "tidy", "-e")
		fmt.Fprintf(out, "• go generate ./gen/ent (entc client)\n")
		if err := runCmd(ctx, abs, nil, "go", "generate", "./gen/ent"); err != nil {
			return nil, fmt.Errorf("go generate ./gen/ent (entc): %w", err)
		}
	}

	fmt.Fprintf(out, "• go mod tidy\n")
	if err := runCmd(ctx, abs, nil, "go", "mod", "tidy"); err != nil {
		return nil, fmt.Errorf("go mod tidy: %w", err)
	}

	return m, nil
}

// preflight requires apx and buf on PATH; go too when generation runs (T-202).
func preflight(ctx context.Context, noGenerate bool) error {
	required := []toolReq{
		{name: "apx", minVersion: "0.12.1", versionArgs: []string{"--version"}},
		{name: "buf", minVersion: "1.40.0", versionArgs: []string{"--version"}},
	}
	if !noGenerate {
		required = append(required, toolReq{name: "go", versionArgs: []string{"version"}})
	}
	var missing []string
	for _, r := range required {
		if _, err := exec.LookPath(r.name); err != nil {
			if r.minVersion != "" {
				missing = append(missing, fmt.Sprintf("%s (>= %s)", r.name, r.minVersion))
			} else {
				missing = append(missing, r.name)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required tool(s) not found on PATH: %s\n"+
			"install apx (https://github.com/infobloxopen/apx) and buf (https://buf.build), then retry",
			strings.Join(missing, ", "))
	}
	return nil
}

type toolReq struct {
	name        string
	minVersion  string
	versionArgs []string
}

// prepareDir creates the target dir and refuses a non-empty one unless force.
func prepareDir(dir string, force bool) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.MkdirAll(dir, 0o755)
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("target %q exists and is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 && !force {
		return fmt.Errorf("target directory %q is not empty (use --force to scaffold into it anyway)", dir)
	}
	return nil
}

// apxInitApp shells out to `apx init app proto/<svc>/v1` (D-2). It writes apx.yaml,
// .gitignore, .github/workflows/apx-release.yml, and an example proto we overwrite.
func apxInitApp(ctx context.Context, dir string, m *Model) error {
	return runCmd(ctx, dir, nil, "apx", "init", "app",
		"proto/"+m.ProtoPathSuffix,
		"--non-interactive",
		"--org", m.Org,
		"--repo", m.RepoName,
	)
}

// runBufGenerate runs `buf generate` with the SDK codegen plugins on PATH. The
// plugin bin dir is resolved by sdkPluginPATH (installed pinned, or overridden
// for in-dev testing via DEVEDGE_SDK_PLUGIN_BIN).
func runBufGenerate(ctx context.Context, dir string) error {
	binDir, err := sdkPluginPATH(ctx, dir)
	if err != nil {
		return err
	}
	env := os.Environ()
	if binDir != "" {
		env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	if err := runCmd(ctx, dir, env, "buf", "dep", "update"); err != nil {
		return fmt.Errorf("buf dep update: %w", err)
	}
	return runCmd(ctx, dir, env, "buf", "generate")
}

func runCmd(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, msg)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
