package devedgesdk_test

import (
	"os/exec"
	"strings"
	"testing"
)

// otelSDKImportGuards is the set of import-path fragments that must NEVER appear
// in the transitive dependency closure of the clean core: the OTel SDK and any
// exporter. The core instruments against the OTel API + contrib handlers, which
// depend on the API only; the SDK + exporters are confined to the single
// observability/otel adapter (mirror of the franz-go/kafkabus discipline).
var otelSDKImportGuards = []string{
	"go.opentelemetry.io/otel/sdk",       // SDK trace/metric/resource impl
	"go.opentelemetry.io/otel/exporters/", // every OTLP/stdout exporter
}

// coreRoots are the package roots that make up the clean core (AC-4). events is
// included as its top-level package only — events/kafkabus is the sanctioned
// broker adapter and is excluded, exactly as observability/otel is the sanctioned
// observability adapter and is the ONE package allowed to import the SDK.
var coreRoots = []string{
	"./server/...",
	"./middleware/...",
	"./authz/...",
	"./persistence/...",
	"./events",
	"./lro/...",
	"./secret/...",
}

// TestCleanCore_NoOTelSDKImport is the load-bearing dependency-light gate (AC-4):
// no core package may reach the OTel SDK or any exporter, directly OR transitively.
//
// The check runs over the FULL transitive closure (`go list -deps`), not direct
// imports: a core package that imported the observability/otel adapter would name
// ".../observability/otel" in its own import list, not "otel/sdk", so a
// direct-import-only check would miss the leak. Because the adapter is never in a
// clean core's closure, ANY appearance of a guarded fragment is an unambiguous leak.
func TestCleanCore_NoOTelSDKImport(t *testing.T) {
	args := append([]string{"list", "-deps"}, coreRoots...)
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps core roots: %v\n%s", err, out)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		for _, guard := range otelSDKImportGuards {
			if strings.Contains(dep, guard) {
				t.Errorf("clean core must not depend on %q (OTel SDK/exporter leak): %q is in the transitive dependency closure", guard, dep)
			}
		}
	}
}

// TestObservabilityAdapter_DoesImportOTelSDK is the converse assertion: the
// observability/otel adapter MUST pull the SDK + exporters (so the gate above is
// meaningful — the deps exist in the module, confined to the adapter, like
// franz-go in kafkabus).
func TestObservabilityAdapter_DoesImportOTelSDK(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./observability/otel/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps observability/otel: %v\n%s", err, out)
	}
	deps := string(out)
	for _, guard := range otelSDKImportGuards {
		if !strings.Contains(deps, guard) {
			t.Errorf("observability/otel adapter is expected to import %q but it is absent from its closure", guard)
		}
	}
}
