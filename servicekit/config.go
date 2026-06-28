package servicekit

import (
	"strings"

	"github.com/infobloxopen/devedge-sdk/config"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// hostConfigStore is the WS-012 P3 config layering (proposal §5.6). The host loads
// ALL configuration from ONE set of sources, then hands each module a
// [ConfigProvider] SCOPED to that module's prefix, so two co-resident modules read
// isolated config slices from the same source set without colliding. A module never
// reads raw global env / parses flags / starts listeners — the host does; the module
// only calls app.Config.Load(&typed) and gets exactly its own slice.
//
// Layering model:
//   - a PLATFORM-GLOBAL layer keyed by the reserved prefixes runtime.*, database.*,
//     observability.*, authz.*, events.* — owned by the host (it builds the server,
//     DB, etc. from these); a module does not read them; and
//   - a PER-MODULE layer keyed by the module's ConfigDescriptor.Prefix (default: the
//     module ID). The provider rewrites a module's config key K to "<PREFIX>_K" (the
//     SCREAMING_SNAKE convention env/flag sources already use), so module "orders"
//     reading config:"GRPC_TIMEOUT" resolves "ORDERS_GRPC_TIMEOUT".
//
// One source set drives both standalone (one module → its prefix) and composed (N
// modules → N prefixes). A single-module host with an empty prefix reads keys
// verbatim, so a standalone service is unchanged.
type hostConfigStore struct {
	sources []config.Source
}

func newConfigStore(sources []config.Source) *hostConfigStore {
	return &hostConfigStore{sources: sources}
}

// providerFor returns a [ConfigProvider] scoped to prefix. A module receives one
// built from its ConfigDescriptor.Prefix (or its ID when the prefix is empty). An
// empty prefix yields a verbatim (unscoped) provider — the standalone path.
func (s *hostConfigStore) providerFor(prefix string) *scopedConfigProvider {
	return &scopedConfigProvider{sources: s.sources, prefix: normalizePrefix(prefix)}
}

// globalProvider returns the host's own unscoped provider for the platform-global
// layer (runtime.*, database.*, …). The host uses it; modules do not.
func (s *hostConfigStore) globalProvider() *scopedConfigProvider {
	return &scopedConfigProvider{sources: s.sources, prefix: ""}
}

// normalizePrefix upper-cases a module prefix and trims separators, so a prefix of
// "orders" / "Orders" / "orders_" all scope to keys "ORDERS_<K>". An empty prefix
// stays empty (verbatim lookups).
func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "._")
	if p == "" {
		return ""
	}
	return strings.ToUpper(strings.ReplaceAll(p, ".", "_"))
}

// scopedConfigProvider is the per-module [ConfigProvider]. Its Load wraps the host's
// shared sources in a prefix-rewriting layer so a module's typed config struct fills
// ONLY that module's slice. It is the P3 replacement for the inert P1 hostConfigProvider.
type scopedConfigProvider struct {
	sources []config.Source
	prefix  string // normalized, "" = verbatim (no scoping)
}

// Load populates dst from the host's sources, scoping every key to this provider's
// module prefix. With an empty prefix it is byte-for-byte the P1 unscoped behavior
// (config.Load over the raw sources). With a prefix it looks each key up as
// "<PREFIX>_<key>" so two modules never read each other's config.
func (p *scopedConfigProvider) Load(dst any) error {
	if p.prefix == "" {
		return config.Load(dst, p.sources...)
	}
	scoped := make([]config.Source, len(p.sources))
	for i, src := range p.sources {
		scoped[i] = prefixSource{prefix: p.prefix + "_", src: src}
	}
	return config.Load(dst, scoped...)
}

// prefixSource rewrites a key lookup to "<prefix><key>" against the underlying
// source. It is how one source set serves N modules: module "orders" sees only the
// "ORDERS_*" keys, module "billing" only "BILLING_*", from the same env/flag/file.
type prefixSource struct {
	prefix string
	src    config.Source
}

func (s prefixSource) Get(key string) (string, bool) {
	return s.src.Get(s.prefix + key)
}

// reservedConfigPrefixes are the platform-global config namespaces the HOST owns
// (proposal §5.6). They are documented here so the host (and validation) can reject a
// module prefix that collides with a reserved one. A module configures itself under
// its own prefix; these belong to the host.
var reservedConfigPrefixes = map[string]struct{}{
	"RUNTIME":       {},
	"DATABASE":      {},
	"OBSERVABILITY": {},
	"AUTHZ":         {},
	"EVENTS":        {},
}

// isReservedConfigPrefix reports whether a normalized module prefix collides with a
// host-owned platform-global namespace.
func isReservedConfigPrefix(normalized string) bool {
	_, ok := reservedConfigPrefixes[normalized]
	return ok
}

// hostDatabaseRegistry is the WS-012 P2 DatabaseRegistry: it allocates a real
// per-module DatabaseNamespace from the host's engine + the host/module isolation
// policy, via persistence.ResolveNamespace (the single allocation rule shared with
// the adapters). It is the host's source of namespace identities; the module reads
// its namespace from App.DB in Register and constructs its namespaced stores from it.
//
// When the host declares NO engine (the single-module / unshared-DB default — no
// HostConfig.Database), the registry returns a zero-qualification namespace (just
// the module ID), so a standalone service is byte-for-byte unchanged. Qualification
// only engages once a host shares one database across modules and declares an engine.
type hostDatabaseRegistry struct {
	engine        string          // the host DB engine (e.g. "postgres"); empty = no shared DB
	defaultPolicy IsolationPolicy // composition default when a module leaves Isolation unset
}

func (r hostDatabaseRegistry) Namespace(moduleID string, d DatabaseDescriptor) (DatabaseNamespace, error) {
	// No shared engine declared: single-module / unshared-DB path — no qualification.
	if r.engine == "" {
		return DatabaseNamespace{ModuleID: moduleID}, nil
	}
	policy := d.Isolation
	if policy == persistence.IsolationUnset {
		policy = r.defaultPolicy
	}
	return persistence.ResolveNamespace(policy, moduleID, r.engine, d.Schema, d.TablePrefix)
}

// hostMetricsRegistry is the WS-012 P3 MetricsRegistry: a THIN per-module-labeled
// wrapper over whatever metrics the SDK already exposes (the OTel seam emits per-RPC
// RED metrics globally). It does NOT add a new metrics backend; it only hands a
// module a stable, collision-free metric namespace derived from its ID, so a module
// that records its own metric labels them under its module namespace. P1's inert
// version returned the ID unchanged; P3 normalizes it to a metric-safe token.
type hostMetricsRegistry struct{}

// Namespace returns a metric-safe per-module namespace token. It lower-cases and
// sanitizes the module ID so it is a valid metric label/prefix component, keeping two
// co-resident modules' metrics distinguishable without introducing a new backend.
func (hostMetricsRegistry) Namespace(moduleID string) string {
	return metricToken(moduleID)
}

// metricToken sanitizes a module ID into a metric-safe lower_snake token.
func metricToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
