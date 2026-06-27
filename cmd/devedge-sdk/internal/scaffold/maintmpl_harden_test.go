package scaffold

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestMainTemplate_FlagBlockValid renders both main entrypoint templates and
// asserts (a) the rendered Go parses, (b) the override flags are actually
// defined on the FlagSet, (c) os.Args is parsed before config.Load, and (d) the
// "os" import is present. This guards the harden(config) fix that wired up the
// previously-dead -GRPC_ADDR/-HTTP_ADDR/etc. flag layer.
func TestMainTemplate_FlagBlockValid(t *testing.T) {
	for _, backend := range []Backend{BackendGORM, BackendEnt} {
		backend := backend
		t.Run(string(backend), func(t *testing.T) {
			m, err := Options{
				Service:  "orders",
				Resource: "Order",
				Backend:  backend,
				Dir:      t.TempDir(),
			}.Validate()
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			src, err := renderTemplate(mainTemplate(backend), m)
			if err != nil {
				t.Fatalf("renderTemplate: %v", err)
			}
			s := string(src)

			// (a) parses as valid Go.
			if _, err := parser.ParseFile(token.NewFileSet(), "main.go", src, parser.AllErrors); err != nil {
				t.Fatalf("rendered main.go does not parse: %v\n---\n%s", err, s)
			}
			// (b) the override flags are defined.
			for _, f := range []string{
				`fs.String("GRPC_ADDR"`,
				`fs.String("HTTP_ADDR"`,
				`fs.String("LOG_LEVEL"`,
				`fs.String("OTLP_ENDPOINT"`,
				`fs.String("DSN"`,
			} {
				if !strings.Contains(s, f) {
					t.Errorf("rendered main.go missing flag definition %q", f)
				}
			}
			// (c) os.Args parsed before config.Load.
			parseIdx := strings.Index(s, "fs.Parse(os.Args[1:])")
			loadIdx := strings.Index(s, "config.Load(")
			if parseIdx < 0 {
				t.Error("rendered main.go does not parse os.Args[1:]")
			}
			if loadIdx < 0 {
				t.Fatal("rendered main.go has no config.Load call")
			}
			if parseIdx > loadIdx {
				t.Error("fs.Parse must run BEFORE config.Load so config.Flags sees set flags")
			}
			// (d) the dead `fs.Parse(nil)` anti-pattern is gone.
			if strings.Contains(s, "fs.Parse(nil)") {
				t.Error("rendered main.go still calls fs.Parse(nil) (parses no args)")
			}
			// (e) "os" import present.
			if !strings.Contains(s, `"os"`) {
				t.Error(`rendered main.go missing "os" import`)
			}
		})
	}
}
