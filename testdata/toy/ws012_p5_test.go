package widgetsv1_test

// ws012_p5_test.go — WS-012 P5 acceptance: wire [servicekittest.AssertModule]
// against the generated WidgetServiceModule (the canonical toy fixture). This
// is the "per-module contract test" from proposal §7: it runs entirely in-process
// (no Docker, no real DB) and verifies the module satisfies the full set of
// contract assertions the harness checks.
//
// This test IS the acceptance proof that AssertModule works against a REAL
// generated module (not just a fake). It passes when the WidgetServiceModule
// descriptor is valid, stable, and the module boots cleanly in an isolated host.

import (
	"testing"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/servicekit"
	"github.com/infobloxopen/devedge-sdk/servicekittest"
	"github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1"
)

// TestP5_AssertModule_WidgetService is the P5 per-module contract acceptance
// proof: AssertModule runs against the fully-generated WidgetServiceModule,
// asserting descriptor validity + stability, config schema load, clean registration,
// and the server's union completeness gate (every method has a rule).
func TestP5_AssertModule_WidgetService(t *testing.T) {
	repo := persistence.NewMemoryRepository[*widgetsv1.Widget, string](
		func(w *widgetsv1.Widget) string { return w.Id },
	)
	mod := widgetsv1.WidgetServiceModule(widgetsv1.WidgetServiceModuleOptions{Repo: repo})

	// No MigrationRunner: the toy module has no migrations (it uses in-memory repo).
	// The harness still asserts descriptor validity + stability + boot gate.
	servicekittest.AssertModule(t, mod)
}

// TestP5_AssertCompatible_WidgetService proves AssertCompatible works against a
// real module. The WidgetServiceModule has no Requires constraints, so it is
// always compatible with any host. We assert that here.
func TestP5_AssertCompatible_WidgetService(t *testing.T) {
	repo := persistence.NewMemoryRepository[*widgetsv1.Widget, string](
		func(w *widgetsv1.Widget) string { return w.Id },
	)
	mod := widgetsv1.WidgetServiceModule(widgetsv1.WidgetServiceModuleOptions{Repo: repo})

	servicekittest.AssertCompatible(t, []servicekit.Module{mod}, servicekittest.HostRequires{
		SDK:      "v0.27.0",
		Go:       "1.25.5",
		Postgres: "",
	})
}
