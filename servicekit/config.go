package servicekit

import (
	"github.com/infobloxopen/devedge-sdk/config"
)

// hostConfigProvider is the P1 ConfigProvider: it loads a module's typed config
// struct from the host's shared config sources via config.Load. It is UNSCOPED in
// P1 (no per-module prefix layering) — that is P3, which will wrap these sources
// in a prefix-scoping layer keyed by the module's ConfigDescriptor.Prefix. A
// single-module host needs no prefixing, so P1's provider serves the standalone
// path correctly.
type hostConfigProvider struct {
	sources []config.Source
}

func newConfigProvider(sources []config.Source) *hostConfigProvider {
	return &hostConfigProvider{sources: sources}
}

// Load populates dst from the host's config sources. P3: scope to a module's
// prefix and overlay a platform-global layer.
func (p *hostConfigProvider) Load(dst any) error {
	return config.Load(dst, p.sources...)
}

// inertDatabaseRegistry is the P1 DatabaseRegistry: it performs NO namespacing.
// It returns the module ID as the namespace identity with no schema/prefix, so a
// single-module (or non-shared-DB) host behaves exactly as today. P2 replaces it
// with a registry that allocates a DatabaseNamespace (schema/prefix/migration
// table) and runs host-owned, advisory-locked migrations. Marked inert per the P1
// scope gate.
type inertDatabaseRegistry struct{}

func (inertDatabaseRegistry) Namespace(moduleID string, _ DatabaseDescriptor) (DatabaseNamespace, error) {
	return DatabaseNamespace{ModuleID: moduleID}, nil
}

// inertEventRegistry is the P1 EventRegistry: it records nothing actionable. P3
// makes the host own one relay + one consumer per module outbox. Marked inert per
// the P1 scope gate.
type inertEventRegistry struct{}

func (inertEventRegistry) RegisterOutbox(_ string, _ OutboxDescriptor) error { return nil }

// inertMetricsRegistry is the P1 MetricsRegistry: the SDK's OTel seam already
// emits per-RPC RED metrics globally, so P1 needs no per-module metrics wiring.
// P3 scopes metrics per module. Marked inert per the P1 scope gate.
type inertMetricsRegistry struct{}

func (inertMetricsRegistry) Namespace(moduleID string) string { return moduleID }
