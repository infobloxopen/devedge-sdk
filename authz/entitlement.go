package authz

import (
	"context"
	"fmt"
	"log/slog"
)

// FeatureSource reports the entitlement features granted to an account
// (tenant). It is the per-tenant entitlement-data seam the in-process gate
// reads: production binds it to the licensing/entitlement service (e.g.
// athena.license.service SKUs), development to [StaticFeatures]. It composes
// with the per-tenant rules substrate (rules.Source) the same way feature flags
// and tags do — a rules.Cache[map[string]bool] satisfies it.
type FeatureSource interface {
	// Granted returns the features enabled for the account. A nil/empty map
	// means "no features", so the gate denies any method that requires one
	// (fail closed).
	Granted(ctx context.Context, account string) (map[string]bool, error)
}

// FeatureSourceFunc adapts a function to a [FeatureSource].
type FeatureSourceFunc func(ctx context.Context, account string) (map[string]bool, error)

// Granted implements [FeatureSource].
func (f FeatureSourceFunc) Granted(ctx context.Context, account string) (map[string]bool, error) {
	return f(ctx, account)
}

// StaticFeatures is an in-memory [FeatureSource] for development and tests: a
// fixed per-account feature set. Not for production — the enterprise binding is
// the licensing/entitlement service.
type StaticFeatures struct {
	byAccount map[string]map[string]bool
}

// NewStaticFeatures builds a [StaticFeatures] from account → granted features.
func NewStaticFeatures(byAccount map[string][]string) *StaticFeatures {
	s := &StaticFeatures{byAccount: make(map[string]map[string]bool, len(byAccount))}
	for acct, feats := range byAccount {
		set := make(map[string]bool, len(feats))
		for _, f := range feats {
			set[f] = true
		}
		s.byAccount[acct] = set
	}
	return s
}

// Granted implements [FeatureSource].
func (s *StaticFeatures) Granted(_ context.Context, account string) (map[string]bool, error) {
	return s.byAccount[account], nil
}

// EntitlementAuthorizer decorates an inner [Authorizer] so the SAME decision
// covers AuthZ (the inner permission check) AND Entitlement (the request's
// required Features) — matching the OPA sidecar's combined rbac+entitlement
// result, but in-process for development. It defers the permission decision to
// inner; if that allows AND the request declares Features, it additionally
// requires every feature be granted to the principal's tenant. It fails closed
// on a missing feature or a source error.
//
// Production wires the OPA Authorizer (which already returns the unified
// decision) and does NOT wrap; the wrap is the dev/in-process path.
type EntitlementAuthorizer struct {
	inner    Authorizer
	features FeatureSource
}

// WithEntitlement returns an [Authorizer] that adds an entitlement (feature)
// check to inner. If features is nil, inner is returned unchanged.
func WithEntitlement(inner Authorizer, features FeatureSource) Authorizer {
	if features == nil {
		return inner
	}
	return &EntitlementAuthorizer{inner: inner, features: features}
}

// Authorize implements [Authorizer].
func (e *EntitlementAuthorizer) Authorize(ctx context.Context, req AccessRequest) (Decision, error) {
	dec, err := e.inner.Authorize(ctx, req)
	if err != nil || !dec.Allow {
		return dec, err
	}
	if len(req.Features) == 0 {
		return dec, nil
	}
	granted, err := e.features.Granted(ctx, req.Principal.Tenant)
	if err != nil {
		// Fail closed: an entitlement-source error must not grant access.
		return Decision{Allow: false, Reason: "entitlement source error"}, fmt.Errorf("authz: entitlement source: %w", err)
	}
	for _, f := range req.Features {
		if !granted[f] {
			return Decision{Allow: false, Reason: fmt.Sprintf("entitlement %q not granted", f)}, nil
		}
	}
	return dec, nil
}

// Alert is the record emitted when a method declared [ModeAlert] fails its
// policy decision but is allowed through (observation mode). It carries enough
// to audit/measure "what would have been denied" without coupling to any sink.
type Alert struct {
	Method    string
	Principal Principal
	Resource  Resource
	Verb      Verb
	Features  []string
	Reason    string // why the decision failed
}

// AlertSink receives alerts from the gate when a [ModeAlert] method would have
// been denied. It is the mechanism; where alerts go (audit, the change feed, a
// metric) is policy supplied by the binding. The default is [LogAlertSink]; an
// enterprise build can forward to the audit pipeline.
type AlertSink interface {
	Emit(ctx context.Context, a Alert)
}

// AlertSinkFunc adapts a function to an [AlertSink].
type AlertSinkFunc func(ctx context.Context, a Alert)

// Emit implements [AlertSink].
func (f AlertSinkFunc) Emit(ctx context.Context, a Alert) { f(ctx, a) }

// LogAlertSink writes each alert as a structured warning. It is the always-safe
// default: no transaction, no I/O dependency, never blocks the request path —
// the authz interceptor runs pre-handler, outside any transaction, so it cannot
// emit onto the outbox change feed directly.
type LogAlertSink struct{ Logger *slog.Logger }

// NewLogAlertSink returns a [LogAlertSink] writing to logger (slog.Default when nil).
func NewLogAlertSink(logger *slog.Logger) *LogAlertSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogAlertSink{Logger: logger}
}

// Emit implements [AlertSink].
func (s *LogAlertSink) Emit(ctx context.Context, a Alert) {
	s.Logger.WarnContext(ctx, "authz: alert-mode policy failure (allowed)",
		"method", a.Method,
		"subject", a.Principal.Subject,
		"tenant", a.Principal.Tenant,
		"verb", string(a.Verb),
		"resource", a.Resource.Type,
		"features", a.Features,
		"reason", a.Reason,
	)
}
