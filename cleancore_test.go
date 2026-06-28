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
	"go.opentelemetry.io/otel/sdk",        // SDK trace/metric/resource impl
	"go.opentelemetry.io/otel/exporters/", // every OTLP/stdout exporter
}

// koanfImportGuards is the set of import-path fragments that must NEVER appear
// in the transitive dependency closure of the core config package. koanf (and
// any heavy config library) is confined to the config/koanf adapter — the core
// config package is stdlib-only.
var koanfImportGuards = []string{
	"github.com/knadh/koanf",
}

// breakerLibImportGuards is the set of circuit-breaker library import-path
// fragments that must NEVER appear in the transitive closure of the clean core.
// The resilience package ships the CircuitBreaker INTERFACE only; concrete libs
// (gobreaker, hystrix-go, etc.) are confined to consumer code.
var breakerLibImportGuards = []string{
	"sony/gobreaker",
	"afex/hystrix-go",
	"eapache/go-resiliency",
}

// ormImportGuards is the set of ORM import-path fragments that must NEVER appear
// in the transitive closure of the clean core (WS-011 / F039 P2). The core
// persistence layer ships INTERFACES + the in-memory + filter/resourcename helpers
// only; the concrete engines — gorm and ent — are confined to the
// persistence/gormtx and persistence/entrepo adapter modules, the exact same
// discipline as OTel SDK vs observability/otel and koanf vs config/koanf. Because
// each adapter is its own Go module, a core package that reached gorm/ent would be
// a COMPILE error across the module boundary, not merely a closure leak — this gate
// catches the regression before the boundary does.
var ormImportGuards = []string{
	"gorm.io/gorm",
	"entgo.io/ent",
}

// coreRoots are the package roots that make up the clean core (AC-4). events is
// included as its top-level package only — events/kafkabus is the sanctioned
// broker adapter and is excluded, exactly as observability/otel is the sanctioned
// observability adapter and is the ONE package allowed to import the SDK.
// The top-level config package is included (stdlib-only seam); config/koanf is
// the sanctioned heavy adapter and is excluded here.
var coreRoots = []string{
	"./server/...",
	"./middleware/...",
	"./authz/...",
	"./persistence/...",
	"./events",
	"./lro/...",
	"./secret/...",
	"./config",
	"./resilience/...",
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

// TestCleanCore_NoKoanfImport guards that the core config package (stdlib-only
// seam) does not transitively pull koanf or any heavy config library. koanf is
// confined to the config/koanf adapter — the exact same discipline as OTel SDK
// vs observability/otel.
func TestCleanCore_NoKoanfImport(t *testing.T) {
	// Check only the core config package (NOT config/koanf which is the adapter).
	out, err := exec.Command("go", "list", "-deps", "./config").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./config: %v\n%s", err, out)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		for _, guard := range koanfImportGuards {
			if strings.Contains(dep, guard) {
				t.Errorf("core config package must not depend on %q (koanf leak): %q is in the transitive dependency closure", guard, dep)
			}
		}
	}
}

// TestCleanCore_NoBreakerLibImport guards that the resilience package (and the
// broader core) does not transitively import any concrete circuit-breaker
// library. The resilience package ships the CircuitBreaker INTERFACE only;
// concrete implementations (sony/gobreaker, afex/hystrix-go, etc.) must be
// supplied by consumers and must never appear in the core's closure.
func TestCleanCore_NoBreakerLibImport(t *testing.T) {
	args := append([]string{"list", "-deps"}, coreRoots...)
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps core roots: %v\n%s", err, out)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		for _, guard := range breakerLibImportGuards {
			if strings.Contains(dep, guard) {
				t.Errorf("clean core must not depend on circuit-breaker lib %q: %q found in transitive closure", guard, dep)
			}
		}
	}
}

// TestKoanfAdapter_DoesImportKoanf is the converse assertion: the config/koanf
// adapter MUST pull koanf (so the guard above is meaningful and the dep is
// genuinely isolated to the adapter).
func TestKoanfAdapter_DoesImportKoanf(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./config/koanf/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./config/koanf: %v\n%s", err, out)
	}
	deps := string(out)
	found := false
	for _, guard := range koanfImportGuards {
		if strings.Contains(deps, guard) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("config/koanf adapter is expected to import koanf but it is absent from its closure")
	}
}

// TestCleanCore_NoORMImport guards that no clean-core package transitively reaches
// gorm or ent (WS-011 / F039 P2). gorm + ent are confined to the persistence/gormtx
// + persistence/entrepo adapter modules; the core persistence layer is ORM-free.
// coreRoots includes ./persistence/... which, after the P2 split, matches ONLY the
// core packages (persistence, filter, resourcename) — gormtx/entrepo are separate
// modules and are excluded from the root module's `./...` patterns.
func TestCleanCore_NoORMImport(t *testing.T) {
	args := append([]string{"list", "-deps"}, coreRoots...)
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps core roots: %v\n%s", err, out)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		for _, guard := range ormImportGuards {
			if strings.Contains(dep, guard) {
				t.Errorf("clean core must not depend on ORM %q (gorm/ent leak): %q is in the transitive dependency closure", guard, dep)
			}
		}
	}
}

// TestGormtxAdapter_DoesImportGorm is the converse assertion: the persistence/gormtx
// adapter MUST pull gorm (so the guard above is meaningful and gorm is genuinely
// isolated to the adapter module).
func TestGormtxAdapter_DoesImportGorm(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./persistence/gormtx/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./persistence/gormtx: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "gorm.io/gorm") {
		t.Errorf("persistence/gormtx adapter is expected to import gorm.io/gorm but it is absent from its closure")
	}
}

// TestEntrepoAdapter_DoesImportEnt is the converse assertion: the persistence/entrepo
// adapter MUST pull ent (so the guard above is meaningful and ent is genuinely
// isolated to the adapter module).
func TestEntrepoAdapter_DoesImportEnt(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./persistence/entrepo/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./persistence/entrepo: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "entgo.io/ent") {
		t.Errorf("persistence/entrepo adapter is expected to import entgo.io/ent but it is absent from its closure")
	}
}
