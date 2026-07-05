package scaffold

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestModuleComposeTemplate_GormValid renders the gorm composable entrypoint
// (module/compose.go) and asserts it parses as Go and exposes the uniform seam a
// composed host (`de compose build`) calls: NewModule(db *gorm.DB) and Models().
// This is the WS-012 contract fix (Run 18 finding 079): the composed host builds
// each member over one shared *gorm.DB via NewModule, so it never names a member's
// repository or model.
func TestModuleComposeTemplate_GormValid(t *testing.T) {
	m, err := Options{
		Service:  "orders",
		Resource: "Order",
		Backend:  BackendGORM,
		Dir:      t.TempDir(),
	}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	src, err := renderTemplate("module_compose.gorm.go.tmpl", m)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	s := string(src)
	if _, err := parser.ParseFile(token.NewFileSet(), "compose.go", src, parser.AllErrors); err != nil {
		t.Fatalf("rendered module/compose.go does not parse: %v\n---\n%s", err, s)
	}
	for _, want := range []string{
		"func NewModule(db *gorm.DB, opts ...ModuleOption) servicekit.Module",
		"func Models() []any",
		"ordersv1.NewOrderRepository(db)",
		"&ordersv1.OrderModel{}",
		// #186: the minter/encryptor injection seam.
		"func WithCredentialMinter(m *secret.CredentialMinter) ModuleOption",
		"func WithEncryptor(enc secret.Encryptor) ModuleOption",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered compose.go missing %q\n---\n%s", want, s)
		}
	}
}

// TestModuleComposeTemplate_EntValid renders the ent composable entrypoint
// (module/compose.go) and asserts it parses as Go and exposes the ent seam a
// composed host calls: NewModule(*ent.Client) and a host-owned CreateSchema
// migration path (#177), plus the minter/encryptor injection option (#186).
func TestModuleComposeTemplate_EntValid(t *testing.T) {
	m, err := Options{
		Service:  "orders",
		Resource: "Order",
		Backend:  BackendEnt,
		Dir:      t.TempDir(),
	}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	src, err := renderTemplate("module_compose.ent.go.tmpl", m)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	s := string(src)
	if _, err := parser.ParseFile(token.NewFileSet(), "compose.go", src, parser.AllErrors); err != nil {
		t.Fatalf("rendered ent module/compose.go does not parse: %v\n---\n%s", err, s)
	}
	for _, want := range []string{
		"func NewModule(client *entclient.Client, opts ...ModuleOption) servicekit.Module",
		"func CreateSchema(ctx context.Context, client *entclient.Client) error",
		"client.Schema.Create(ctx)",
		"ordersv1.NewOrderEntRepository(client)",
		"func WithCredentialMinter(m *secret.CredentialMinter) ModuleOption",
		"func WithEncryptor(enc secret.Encryptor) ModuleOption",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered ent compose.go missing %q\n---\n%s", want, s)
		}
	}
}
