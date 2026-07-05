package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionCommand_PrintsNonEmptyVersion is the acceptance test for issue
// #108: `devedge-sdk version` must print a non-empty version string mentioning
// the binary name, so a consumer can verify what they installed without
// resorting to `go version -m`. Hermetic: it only exercises the root command's
// wiring, no network or external process.
func TestVersionCommand_PrintsNonEmptyVersion(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute version command: %v", err)
	}

	got := out.String()
	if strings.TrimSpace(got) == "" {
		t.Fatal("version command printed no output")
	}
	if !strings.Contains(got, "devedge-sdk") {
		t.Fatalf("version output %q does not mention devedge-sdk", got)
	}
}

// TestResolveCLIVersion_NeverEmpty guards the fallback chain in
// resolveCLIVersion: whatever the build-info shape under `go test`, callers
// must always get a usable (non-empty) string, even if it is the "(devel)"
// placeholder.
func TestResolveCLIVersion_NeverEmpty(t *testing.T) {
	if v := resolveCLIVersion(); v == "" {
		t.Fatal("resolveCLIVersion returned an empty string")
	}
}
