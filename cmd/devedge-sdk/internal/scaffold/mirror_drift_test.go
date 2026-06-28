package scaffold

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMirrorsMatchSDKSource guards against mirror drift (F028 failure mode): the
// annotation .proto files embedded in this CLI must be byte-identical to the
// SDK's proto/infoblox source they are vendored from, so a scaffold never
// generates against a stale annotation schema.
func TestMirrorsMatchSDKSource(t *testing.T) {
	sdkDir := sdkModuleDir(t)
	embedded, err := MirrorFiles()
	if err != nil {
		t.Fatalf("MirrorFiles: %v", err)
	}
	if len(embedded) == 0 {
		t.Fatal("no embedded mirrors found")
	}
	for rel, want := range embedded {
		// rel looks like "infoblox/authz/v1/authz.proto"
		src := filepath.Join(sdkDir, "proto", rel)
		got, err := os.ReadFile(src)
		if err != nil {
			t.Errorf("read SDK source %s: %v", src, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("embedded mirror %s drifted from SDK source %s; re-copy the file (see cmd/devedge-sdk/internal/scaffold/mirrors)", rel, src)
		}
	}
}

// sdkModuleDir returns the ROOT devedge-sdk module dir. It names the module
// explicitly: the repo is a multi-module workspace (WS-011), and under go.work a
// bare `go list -m -f {{.Dir}}` prints a line per workspace module, so the root
// must be queried by path to get just its tree.
func sdkModuleDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/infobloxopen/devedge-sdk").Output()
	if err != nil {
		t.Fatalf("locate SDK module dir: %v", err)
	}
	return strings.TrimSpace(string(out))
}
