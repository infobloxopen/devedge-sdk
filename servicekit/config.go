package servicekit

import (
	"github.com/infobloxopen/devedge-sdk/config"
	"github.com/infobloxopen/devedge-sdk/persistence"
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
