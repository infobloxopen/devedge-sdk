package authz

// MethodRule is the declared authorization requirement for one API method (e.g.
// a gRPC FullMethod). It is the single declaration that drives BOTH enforcement
// (the interceptor's rule table) AND the generated permission catalog — declare
// once, consume in several places.
//
// In the end state these are produced from proto annotations
// (infoblox.authz.v1.Rule, see proto/); until the generator lands they may be
// declared directly in code.
type MethodRule struct {
	Method   string // transport method id, e.g. "/dns.v1.ZoneService/GetZone"
	Verb     Verb   // the required verb; empty iff Public
	Resource string // resource type or template, e.g. "zone" or "zone:{zone_id}"
	Public   bool   // explicit no-authorization opt-out
}
