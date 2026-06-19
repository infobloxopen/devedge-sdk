package scaffold

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// reconcileApxModuleRoots verifies that the buf module path (proto) is aligned
// with apx.yaml's module_roots (T-203, reality #1).
//
// Layout decision (validated against apx 0.12.1):
//   - apx init app writes module_roots: [proto]; ONE buf module rooted at `proto`
//     holds both your service proto (proto/<svc>/v1) and the vendored annotation
//     mirrors (proto/infoblox/...).
//   - buf.yaml's lint/breaking `ignore: [proto/infoblox]` keeps apx governing only
//     YOUR public proto; apx release prepare targets a single API id, so it never
//     tries to release the mirrors. No mutation of apx.yaml is needed — this
//     function asserts the assumption still holds and fails loudly if apx changes
//     the layout in a future version (failure mode: apx init layout drift).
func reconcileApxModuleRoots(dir string) error {
	apxYAML := filepath.Join(dir, "apx.yaml")
	data, err := os.ReadFile(apxYAML)
	if err != nil {
		return fmt.Errorf("read apx.yaml: %w", err)
	}
	if !hasProtoModuleRoot(string(data)) {
		return fmt.Errorf("apx.yaml module_roots does not include 'proto'; the buf layout assumes it "+
			"(apx may have changed its app layout — see %s)", apxYAML)
	}
	return nil
}

// hasProtoModuleRoot reports whether the apx.yaml text declares `proto` under
// module_roots. Minimal, dependency-free YAML scan (the block apx writes is
// `module_roots:\n    - proto`).
func hasProtoModuleRoot(s string) bool {
	lines := strings.Split(s, "\n")
	inRoots := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "module_roots:") {
			inRoots = true
			continue
		}
		if inRoots {
			if strings.HasPrefix(trimmed, "- ") {
				if strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")) == "proto" {
					return true
				}
				continue
			}
			// left the list block
			if trimmed != "" {
				inRoots = false
			}
		}
	}
	return false
}

// sdkPluginPATH returns a directory containing the SDK codegen plugins
// (protoc-gen-{devedge-authz,svc,storage}) for buf to find on PATH.
//
// Resolution order:
//  1. DEVEDGE_SDK_PLUGIN_BIN — a prebuilt bin dir (used by the in-repo
//     integration test to exercise HEAD instead of the module proxy).
//  2. go install of the plugins pinned to the resolved SDK version into a
//     per-version cache dir under the user's cache (real scaffolds).
//
// The public plugins (protoc-gen-{go,go-grpc,grpc-gateway}) are expected on the
// user's existing PATH (installed by `make tools`).
func sdkPluginPATH(ctx context.Context, scaffoldDir string) (string, error) {
	if override := os.Getenv("DEVEDGE_SDK_PLUGIN_BIN"); override != "" {
		return override, nil
	}

	version := resolveSDKVersion()
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		cacheRoot = os.TempDir()
	}
	binDir := filepath.Join(cacheRoot, "devedge-sdk", "plugins", version)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}

	plugins := []string{
		"github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-devedge-authz",
		"github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-svc",
		"github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-storage",
	}
	for _, p := range plugins {
		name := p[strings.LastIndex(p, "/")+1:]
		if _, err := os.Stat(filepath.Join(binDir, name)); err == nil {
			continue // cached
		}
		cmd := exec.CommandContext(ctx, "go", "install", p+"@"+version)
		cmd.Dir = scaffoldDir
		cmd.Env = append(os.Environ(), "GOBIN="+binDir)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("install %s@%s: %w\n%s", p, version, err, strings.TrimSpace(stderr.String()))
		}
	}
	return binDir, nil
}
