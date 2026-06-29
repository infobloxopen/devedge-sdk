// Package rules is the per-tenant rules-distribution substrate (seam P3a): a
// pluggable [Source] that delivers a typed ruleset per tenant and streams
// changes, plus a fail-safe local [Cache] that keeps a last-known-good snapshot
// so consumers evaluate locally — never a per-operation call to a rules service.
//
// It is the shared machinery beneath ility evaluators such as feature flags
// (a Source[featureflags.FlagSet]) and tag validation (a Source of tag
// definitions): a synced per-tenant snapshot plus an evaluator on top. The
// Watch contract deliberately mirrors cells.RoutingTable.Watch — the proven
// read-mostly fan-out pattern already in the SDK.
//
// Mechanism, not policy: this package distributes and caches whatever ruleset
// type a consumer parameterises it with; it does not interpret rules. The
// rules service that authors them, and the evaluator that applies them, live
// elsewhere (an evaluator in a public package such as featureflags; the
// authoring service in a separate repo, bound to a Source the same way an
// external adapter implements the authz.Authorizer seam).
//
// The package depends only on the standard library and the SDK's health seam.
// Heavy transports — fsnotify, OPA, a Kubernetes ConfigMap bridge, Kafka — are
// separate adapters built just-in-time, never in core, so the root module stays
// dependency-light.
package rules

import (
	"context"
	"errors"
)

// ErrNotFound is returned by [Source.Get] when a tenant has no ruleset. Callers
// treat it as "use the default", not as an error.
var ErrNotFound = errors.New("rules: no ruleset for tenant")

// Event is a single change observed on a [Source.Watch] stream.
type Event[T any] struct {
	// Tenant is the affected tenant. The empty string denotes the global/default
	// ruleset that applies when a tenant has no entry of its own.
	Tenant string
	// Value is the tenant's new ruleset (the zero value of T when Deleted).
	Value T
	// Deleted reports that the tenant's ruleset was removed; consumers fall back
	// to the default ruleset.
	Deleted bool
	// Revision is the source-global monotonic revision at which this change
	// applied. It only ever increases.
	Revision uint64
}

// Source delivers a typed ruleset T per tenant and streams changes. It is the
// pluggable transport seam (P3a): the in-memory [StaticSource] is the dev/test
// default and [FileSource] is the zero-dependency file-backed default; a
// ConfigMap bridge, an OPA-backed source, or a Kafka-backed source implement
// the same interface in adapters.
//
// Implementations must be safe for concurrent use.
type Source[T any] interface {
	// Get returns the current ruleset for tenant, or ErrNotFound when the tenant
	// has no explicit ruleset.
	Get(ctx context.Context, tenant string) (T, error)

	// Watch returns a channel of changes. The channel is closed when ctx is
	// cancelled. Implementations may coalesce rapid updates but must always
	// deliver the latest state for any changed tenant. Mirrors
	// cells.RoutingTable.Watch.
	Watch(ctx context.Context) (<-chan Event[T], error)
}

// Snapshotter is an optional [Source] capability: a bulk read of every tenant's
// ruleset at a revision. [Cache] uses it for the initial load so the cache is
// ready with complete data before it serves, then switches to Watch for
// incremental updates (the standard list-then-watch pattern). Sources that can
// enumerate their tenants — StaticSource, FileSource, a ConfigMap bridge —
// implement it; a pure event stream need not, in which case the Cache becomes
// ready once its Watch subscription is established.
type Snapshotter[T any] interface {
	// Snapshot returns every tenant's ruleset and the revision the snapshot was
	// taken at. The returned map is owned by the caller.
	Snapshot(ctx context.Context) (map[string]T, uint64, error)
}
