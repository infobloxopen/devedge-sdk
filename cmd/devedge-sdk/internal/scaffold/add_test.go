package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExistingService writes the minimum of a devedge-sdk service repo that
// AddDeploy detects from: a go.mod (module + backend dep) and a cmd/<bin> dir.
func fakeExistingService(t *testing.T, module, bin string, backend Backend) string {
	t.Helper()
	dir := t.TempDir()
	dep := "gorm.io/gorm v1.31.1"
	if backend == BackendEnt {
		dep = "entgo.io/ent v0.14.6"
	}
	gomod := "module " + module + "\n\ngo 1.25\n\nrequire " + dep + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cmd", bin), 0o755); err != nil {
		t.Fatal(err)
	}
	// A minimal devedge-sdk-style Makefile (with the `gen` target the image target
	// orders against) so the retrofit's `make image` append has somewhere to go.
	mk := ".PHONY: gen\ngen:\n\t@true\n"
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(mk), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAddDeploy_RetrofitsExistingService is the core retrofit gate: into an
// existing repo with no deploy artifacts, `add deploy` writes the image + deploy
// files, detecting the module/binary/backend from the repo.
func TestAddDeploy_RetrofitsExistingService(t *testing.T) {
	dir := fakeExistingService(t, "github.com/infobloxopen/orders", "orders", BackendGORM)

	res, err := AddDeploy(AddDeployOptions{Dir: dir}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AddDeploy: %v", err)
	}
	if res.Service != "Orders" || res.Module != "github.com/infobloxopen/orders" || res.Backend != BackendGORM {
		t.Fatalf("detected identity wrong: %+v", res)
	}
	for _, rel := range []string{
		".github/workflows/image.yml",
		"deploy/k8s/oci-repository.yaml",
		"deploy/k8s/helmrelease.yaml",
		"deploy/k8s/values.yaml",
		"deploy/k8s/README.md",
		"deploy/compose/docker-compose.yml",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected retrofitted file %s: %v", rel, err)
		}
	}
	// No Dockerfile (ko builds the image).
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); !os.IsNotExist(err) {
		t.Errorf("retrofit must not write a Dockerfile (ko builds the image)")
	}
	// Spot-check the image identity wired to this repo.
	wf := readRendered(t, dir, ".github/workflows/image.yml")
	if !strings.Contains(wf, "./cmd/orders") || !strings.Contains(wf, "ghcr.io/${{ github.repository }}") {
		t.Errorf("image.yml not wired to this repo's binary:\n%s", wf)
	}
	// The local-build `make image` target (socket autodetect) is appended to the Makefile.
	mk := readRendered(t, dir, "Makefile")
	if !strings.Contains(mk, "\nimage:") || !strings.Contains(mk, "docker context inspect") {
		t.Errorf("retrofit did not add a socket-autodetecting `make image` target:\n%s", mk)
	}
	foundMk := false
	for _, f := range res.Written {
		if strings.Contains(f, "Makefile") {
			foundMk = true
		}
	}
	if !foundMk {
		t.Errorf("result should report the Makefile image-target addition: %v", res.Written)
	}
	overlay := readRendered(t, dir, "deploy/k8s/values.yaml")
	if !strings.Contains(overlay, "ghcr.io/infobloxopen/orders") {
		t.Errorf("k8s overlay image.repository not derived from the module:\n%s", overlay)
	}
}

// TestAddDeploy_ImageOnly skips the deploy/ targets.
func TestAddDeploy_ImageOnly(t *testing.T) {
	dir := fakeExistingService(t, "github.com/acme/widgets", "widgets", BackendGORM)
	if _, err := AddDeploy(AddDeployOptions{Dir: dir, ImageOnly: true}, &bytes.Buffer{}); err != nil {
		t.Fatalf("AddDeploy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".github/workflows/image.yml")); err != nil {
		t.Errorf("image.yml should be written with --image-only: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy")); !os.IsNotExist(err) {
		t.Errorf("deploy/ must NOT be written with --image-only")
	}
}

// TestAddDeploy_Idempotent guards the safety property: a re-run skips files that
// already exist (so a service's tree is never clobbered), and --force overwrites.
func TestAddDeploy_Idempotent(t *testing.T) {
	dir := fakeExistingService(t, "github.com/infobloxopen/orders", "orders", BackendGORM)

	first, err := AddDeploy(AddDeployOptions{Dir: dir}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AddDeploy #1: %v", err)
	}
	if len(first.Written) == 0 || len(first.Skipped) != 0 {
		t.Fatalf("first run should write everything and skip nothing: %+v", first)
	}

	// Hand-edit a file to prove the re-run does not clobber it.
	wfPath := filepath.Join(dir, ".github/workflows/image.yml")
	if err := os.WriteFile(wfPath, []byte("# HAND EDITED\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := AddDeploy(AddDeployOptions{Dir: dir}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AddDeploy #2: %v", err)
	}
	if len(second.Written) != 0 {
		t.Errorf("re-run should write nothing, wrote: %v", second.Written)
	}
	if len(second.Skipped) == 0 {
		t.Errorf("re-run should skip the existing artifacts, but skipped none")
	}
	if got := readRendered(t, dir, ".github/workflows/image.yml"); got != "# HAND EDITED\n" {
		t.Errorf("re-run clobbered a hand-edited file:\n%s", got)
	}

	// --force overwrites the hand-edited file back to the rendered workflow.
	forced, err := AddDeploy(AddDeployOptions{Dir: dir, Force: true}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("AddDeploy --force: %v", err)
	}
	if len(forced.Skipped) != 0 {
		t.Errorf("--force should skip nothing, skipped: %v", forced.Skipped)
	}
	if got := readRendered(t, dir, ".github/workflows/image.yml"); !strings.Contains(got, "ko build --bare") {
		t.Errorf("--force did not restore the rendered workflow:\n%s", got)
	}
}

// TestAddDeploy_ByteIdenticalToNewService is the central guarantee the user asked
// for: a retrofitted existing service gets artifacts BYTE-IDENTICAL to a freshly
// scaffolded one. Render the same "orders" service both ways and diff every shared
// artifact.
func TestAddDeploy_ByteIdenticalToNewService(t *testing.T) {
	// New-service render (module defaults to github.com/infobloxopen/orders).
	m, err := Options{Service: "orders", Resource: "Order", Backend: BackendGORM, Dir: t.TempDir()}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	newDir := t.TempDir()
	if err := renderTemplates(newDir, m); err != nil {
		t.Fatalf("renderTemplates: %v", err)
	}

	// Retrofit render of the same service onto an existing repo.
	addDir := fakeExistingService(t, "github.com/infobloxopen/orders", "orders", BackendGORM)
	if _, err := AddDeploy(AddDeployOptions{Dir: addDir}, &bytes.Buffer{}); err != nil {
		t.Fatalf("AddDeploy: %v", err)
	}

	for _, rel := range []string{
		".github/workflows/image.yml",
		"deploy/k8s/oci-repository.yaml",
		"deploy/k8s/helmrelease.yaml",
		"deploy/k8s/values.yaml",
		"deploy/k8s/README.md",
		"deploy/compose/docker-compose.yml",
	} {
		want, err := os.ReadFile(filepath.Join(newDir, rel))
		if err != nil {
			t.Fatalf("read new-service %s: %v", rel, err)
		}
		got, err := os.ReadFile(filepath.Join(addDir, rel))
		if err != nil {
			t.Fatalf("read retrofit %s: %v", rel, err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s differs between new-service and retrofit (must be byte-identical)", rel)
		}
	}
}

// TestAddDeploy_Detection covers backend + binary detection and the override/error
// paths.
func TestAddDeploy_Detection(t *testing.T) {
	t.Run("ent backend from go.mod", func(t *testing.T) {
		dir := fakeExistingService(t, "github.com/infobloxopen/iam", "iam", BackendEnt)
		res, err := AddDeploy(AddDeployOptions{Dir: dir, ImageOnly: true}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("AddDeploy: %v", err)
		}
		if res.Backend != BackendEnt {
			t.Errorf("backend = %q, want ent", res.Backend)
		}
	})

	t.Run("name override", func(t *testing.T) {
		dir := fakeExistingService(t, "github.com/acme/svc", "svc", BackendGORM)
		// Add a second cmd dir so auto-detect would be ambiguous; --name disambiguates.
		if err := os.MkdirAll(filepath.Join(dir, "cmd", "worker"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := AddDeploy(AddDeployOptions{Dir: dir, Name: "svc", ImageOnly: true}, &bytes.Buffer{}); err != nil {
			t.Fatalf("AddDeploy: %v", err)
		}
		wf := readRendered(t, dir, ".github/workflows/image.yml")
		if !strings.Contains(wf, "./cmd/svc") {
			t.Errorf("--name not honored in image.yml:\n%s", wf)
		}
	})

	t.Run("ambiguous binary errors", func(t *testing.T) {
		dir := fakeExistingService(t, "github.com/acme/svc", "svc", BackendGORM)
		if err := os.MkdirAll(filepath.Join(dir, "cmd", "worker"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := AddDeploy(AddDeployOptions{Dir: dir}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "multiple binaries") {
			t.Errorf("expected a multiple-binaries error, got: %v", err)
		}
	})

	t.Run("missing go.mod errors", func(t *testing.T) {
		dir := t.TempDir() // no go.mod
		if err := os.MkdirAll(filepath.Join(dir, "cmd", "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := AddDeploy(AddDeployOptions{Dir: dir}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "go.mod") {
			t.Errorf("expected a go.mod error, got: %v", err)
		}
	})
}
