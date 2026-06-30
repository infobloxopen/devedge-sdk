package authz

// MethodRule is the declared authorization requirement for one API method (e.g.
// a gRPC FullMethod). It is the single declaration that drives enforcement (the
// interceptor's rule table), the generated permission catalog, AND — in the
// unified policy gate (P12) — the method's entitlement (Features) and usage
// (Quota) requirements: declare once, consume in several places.
//
// In the end state these are produced from proto annotations
// (infoblox.authz.v1.Rule + the entitlement/quota annotations, see proto/);
// until the generator lands they may be declared directly in code.
type MethodRule struct {
	Method   string // transport method id, e.g. "/dns.v1.ZoneService/GetZone"
	Verb     Verb   // the required verb; empty iff Public
	Resource string // resource type or template, e.g. "zone" or "zone:{zone_id}"
	Public   bool   // explicit no-authorization opt-out

	// Features are the entitlement features the caller's account must hold for
	// this method (P12). The gate evaluates them in the SAME decision as the
	// permission (AuthZ ∧ Entitlement): see [EntitlementAuthorizer] for the
	// in-process dev backend; the OPA sidecar already returns the combined
	// rbac+entitlement decision. Empty = no entitlement requirement.
	Features []string
	// Mode selects how a failed decision is handled: [ModeEnforce] (the default,
	// and the zero value) denies; [ModeAlert] allows through but emits an
	// [Alert] — for rolling a new requirement out in observation mode.
	Mode Mode
	// Quota, when set, declares a metered unit this method consumes, enforced by
	// the separate usage-metering seam (quota.Meter, P13) around the handler —
	// NOT by the authz decision.
	Quota *QuotaRule
}

// Mode selects how the gate handles a method whose policy decision fails.
type Mode string

const (
	// ModeEnforce denies a failed decision with PermissionDenied. It is the
	// default; the zero value ("") is treated as enforce.
	ModeEnforce Mode = "enforce"
	// ModeAlert allows the request through on a failed decision but emits an
	// [Alert] via the configured [AlertSink]. Used to observe what a new
	// permission/entitlement requirement WOULD deny before turning it on.
	ModeAlert Mode = "alert"
)

// QuotaRule declares that calls to a method consume a metered unit. Enforcement
// is the separate quota.Meter seam (P13), applied around the handler with a
// reserve→commit/release lifecycle — a boolean PDP decision cannot both
// pre-check a limit and consume-only-on-success, so quota is intentionally NOT
// part of the authz decision.
type QuotaRule struct {
	Metric string // the counted unit, e.g. "sandboxes"
	Window string // rate window (e.g. "month"); empty = stock/count (no window)
}
