// Package scaffold renders and assembles an apx-native devedge-sdk service
// project from a service name + one resource. See specs/028-apx-native-scaffold.
package scaffold

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
)

// Backend selects the persistence code generator.
type Backend string

const (
	BackendGORM Backend = "gorm"
	BackendEnt  Backend = "ent"
)

// fallbackSDKVersion is used when the running binary has no embedded module
// version (e.g. built with `go build` from a checkout rather than `go install
// ...@vX`). It pins the generated project's go.mod devedge-sdk require AND the
// scaffold's first plugin install at the SAME version, so a brand-new project is
// internally consistent. After scaffolding, the generated Makefile derives
// SDK_VERSION from go.mod at make-time (go.mod is the single source of truth), so
// this constant only ever sets the INITIAL pin — but a stale value would still
// pin a new project to an old SDK, so keep it aligned with the latest released
// SDK tag (bump on every release).
const fallbackSDKVersion = "v0.18.0"

// Pinned dependency versions for the generated go.mod. These mirror the versions
// the SDK's own testdata modules build against; `go mod tidy` reconciles indirects.
const (
	glebarezSQLiteVersion = "v1.11.0"
	grpcGatewayVersion    = "v2.27.7"
	authzBindingVersion   = "v1.0.0-alpha.4"
	genprotoAPIVersion    = "v0.0.0-20260226221140-a57be14db171"
	grpcVersion           = "v1.81.1"
	protobufVersion       = "v1.36.11"
	gormVersion           = "v1.31.1"
	// ent backend deps (mirror testdata/apikey + testdata/fleet).
	entVersion           = "v0.14.6"
	moderncSQLiteVersion = "v1.52.0"
	fieldBindingVersion  = "v1.0.0-alpha.1"
	// Observability (F034): the OTel API the generated main pulls through the SDK's
	// observability/otel adapter + server seam. Kept in lockstep with the SDK's own
	// go.mod (API v1.x, contrib v0.x); `go mod tidy` reconciles the SDK/exporter
	// indirects. The adapter itself ships INSIDE the devedge-sdk module, so the
	// generated project needs no separate adapter require.
	otelAPIVersion = "v1.44.0"
)

// Options are the user-supplied inputs to a scaffold.
type Options struct {
	// Service is the bare service name (e.g. "orders"); the resource defaults
	// from it when --resource is omitted.
	Service string
	// Resource is the singular resource type name (e.g. "Order").
	Resource string
	// Module is the full Go module path (e.g. "github.com/acme/orders"). When
	// empty it is derived from Org + Service.
	Module string
	// Org is the apx organization (e.g. "infobloxopen").
	Org string
	// Backend selects the persistence generator.
	Backend Backend
	// Dir is the target directory the project is generated into.
	Dir string
	// NoGenerate skips the first `buf generate` + `go mod tidy`.
	NoGenerate bool
	// Force allows generating into a non-empty directory.
	Force bool
	// Aggregate scaffolds the resource as a DDD aggregate ROOT that owns a member
	// (containment) resource, and wires the aggregate + transactional-outbox
	// machinery in the generated main (a TxRunner, an AggregateRepository over the
	// generated graph-load primitive, and an outbox Publisher + Dispatcher). When
	// false the scaffold emits the plain Tier-1 CRUD service (byte-stable default).
	Aggregate bool
}

// Model is the fully-resolved template data. Field names are referenced by the
// templates under templates/*.tmpl.
type Model struct {
	Service        string // PascalCase service name, e.g. "Orders"
	ServiceType    string // proto/Go service type, e.g. "OrderService" (= Resource + "Service")
	Resource       string // PascalCase resource, e.g. "Order"
	ResourceSnake  string // snake_case resource, e.g. "order"
	ResourcePlural string // lower plural, e.g. "orders"
	ServiceLower   string // lower service name, e.g. "orders"

	// Aggregate is true when the resource is scaffolded as a DDD aggregate root
	// (gates the aggregate + outbox wiring in the templates). The Member* fields
	// describe the owned containment member that gives the root a graph to load.
	Aggregate   bool
	Member      string // PascalCase member resource type, e.g. "OrderItem"
	MemberSnake string // snake_case member type, e.g. "order_item"
	// MemberField is the root's repeated containment field name (a SINGLE word, so
	// the proto field, the ent edge, and entc's camelCased accessor all agree —
	// mirrors fleet's `vehicles`). The proto field is its lower-case form, the Go
	// accessor is MemberField + Get.
	MemberField      string // Go field name on the root proto, e.g. "Items"
	MemberFieldProto string // proto field name (lower), e.g. "items"
	MemberFieldFK    string // FK column on the member pointing back at the root, e.g. "order_id"
	MemberFKGoField  string // Go field name of that FK on the member proto, e.g. "OrderId"
	Module           string // go module path
	Org              string // apx org
	RepoName         string // apx repo name (last path element of Module)
	Backend          Backend
	BinName          string // server binary name, e.g. "orders"

	ProtoPackage    string // proto package, e.g. "orders.v1"
	ProtoPathSuffix string // path of the .proto SOURCE under proto/, e.g. "orders/v1"
	ProtoFile       string // proto file name, e.g. "orders.proto"
	GoPkg           string // generated Go package name/alias, e.g. "ordersv1"
	// GoImportPath is the import path of the generated Go package, e.g.
	// "github.com/acme/orders/gen/ordersv1". It is a SINGLE directory segment
	// under gen/ ("gen/<svc>v1"), NOT "gen/<svc>/v1": protoc-gen-ent (which takes
	// no module= opt) emits its ent/ schema package as a sibling of the proto's
	// Go package dir (path.Dir(go_package)+"/ent"), so the generated repository
	// adapter and the ent client only line up when the proto package is one
	// segment deep. The same layout is used for both backends so there is a
	// single convention. (This is why go_package can't be "proto/<svc>/v1", which
	// is what apx would prefer — see the buf.gen templates + tasks.md T-501 note.)
	GoImportPath string

	GRPCPort string
	HTTPPort string

	SDKVersion            string
	GlebarezSQLiteVersion string
	GRPCGatewayVersion    string
	AuthzBindingVersion   string
	GenprotoAPIVersion    string
	GRPCVersion           string
	ProtobufVersion       string
	GormVersion           string
	EntVersion            string
	ModerncSQLiteVersion  string
	FieldBindingVersion   string
	OTelAPIVersion        string
}

var (
	identRe       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	modulePathRe  = regexp.MustCompile(`^[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)+$`)
	serviceNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
)

// Validate checks the options and returns a resolved Model.
func (o Options) Validate() (*Model, error) {
	svc := strings.TrimSpace(o.Service)
	if svc == "" {
		return nil, fmt.Errorf("service name is required")
	}
	if !serviceNameRe.MatchString(svc) {
		return nil, fmt.Errorf("service name %q must be lowercase alphanumeric starting with a letter (e.g. \"orders\")", svc)
	}

	res := strings.TrimSpace(o.Resource)
	if res == "" {
		res = pascal(svc)
		// Singularize a trailing "s" so `--resource` defaults sensibly from a
		// plural service name (orders -> Order).
		if strings.HasSuffix(res, "s") && len(res) > 1 {
			res = strings.TrimSuffix(res, "s")
		}
	}
	if !identRe.MatchString(res) {
		return nil, fmt.Errorf("resource %q must be a Go identifier (letters and digits, starting with a letter, e.g. \"Order\")", res)
	}
	res = pascal(res)

	switch o.Backend {
	case BackendGORM, BackendEnt:
	case "":
		return nil, fmt.Errorf("backend is required")
	default:
		return nil, fmt.Errorf("unknown backend %q (want gorm or ent)", o.Backend)
	}

	org := strings.TrimSpace(o.Org)
	if org == "" {
		org = "infobloxopen"
	}

	module := strings.TrimSpace(o.Module)
	if module == "" {
		module = fmt.Sprintf("github.com/%s/%s", org, svc)
	}
	if !modulePathRe.MatchString(module) {
		return nil, fmt.Errorf("module path %q is not a valid Go module path (e.g. github.com/acme/orders)", module)
	}

	repoName := module[strings.LastIndex(module, "/")+1:]

	// An aggregate root owns a member (containment) resource so the generated
	// Load<Root>Aggregate graph-load primitive is emitted (it is only generated for
	// a root that has at least one containment member). The member is named
	// "<Resource>Item" so it never collides with the root's own type/columns.
	member := res + "Item"

	m := &Model{
		Service:        pascal(svc),
		ServiceType:    res + "Service",
		Resource:       res,
		ResourceSnake:  snake(res),
		ResourcePlural: snake(res) + "s",
		ServiceLower:   svc,

		Aggregate:        o.Aggregate,
		Member:           member,
		MemberSnake:      snake(member),
		MemberField:      "Items",
		MemberFieldProto: "items",
		MemberFieldFK:    snake(res) + "_id",
		MemberFKGoField:  res + "Id",
		Module:           module,
		Org:              org,
		RepoName:         repoName,
		Backend:          o.Backend,
		BinName:          svc,
		ProtoPackage:     svc + ".v1",
		ProtoPathSuffix:  svc + "/v1",
		ProtoFile:        svc + ".proto",
		GoPkg:            svc + "v1",
		GoImportPath:     module + "/gen/" + svc + "v1",
		GRPCPort:         "9090",
		HTTPPort:         "8080",

		SDKVersion:            resolveSDKVersion(),
		GlebarezSQLiteVersion: glebarezSQLiteVersion,
		GRPCGatewayVersion:    grpcGatewayVersion,
		AuthzBindingVersion:   authzBindingVersion,
		GenprotoAPIVersion:    genprotoAPIVersion,
		GRPCVersion:           grpcVersion,
		ProtobufVersion:       protobufVersion,
		GormVersion:           gormVersion,
		EntVersion:            entVersion,
		ModerncSQLiteVersion:  moderncSQLiteVersion,
		FieldBindingVersion:   fieldBindingVersion,
		OTelAPIVersion:        otelAPIVersion,
	}
	return m, nil
}

// resolveSDKVersion returns the version of the devedge-sdk module this binary
// was built from, so the scaffold pins the matching plugin + runtime version
// (D-4: mirror pinned to the resolved SDK version). Falls back to a constant.
func resolveSDKVersion() string {
	if v := sdkVersionFromBuildInfo(); v != "" {
		return v
	}
	return fallbackSDKVersion
}

func sdkVersionFromBuildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	const sdkPath = "github.com/infobloxopen/devedge-sdk"
	if info.Main.Path == sdkPath && isSemver(info.Main.Version) {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path == sdkPath && isSemver(dep.Version) {
			return dep.Version
		}
	}
	return ""
}

func isSemver(v string) bool {
	return strings.HasPrefix(v, "v") && !strings.Contains(v, "devel") && v != "(devel)"
}

// pascal converts a snake/lower name to PascalCase.
func pascal(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	if b.Len() == 0 {
		return s
	}
	return b.String()
}

// snake converts a PascalCase/camelCase name to snake_case.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
