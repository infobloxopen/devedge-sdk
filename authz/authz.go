// Package authz defines a clean, transport-neutral, pluggable authorization
// model for Infoblox services.
//
// The decision point is an [Authorizer]. The SDK ships a development
// implementation ([DevAuthorizer]); production wires a different Authorizer
// (OPA, Cedar, a remote PDP, ...) WITHOUT changing service code. The model is
// deliberately free of any engine- or policy-specific types — those belong in
// adapters outside this SDK, not here.
package authz

import (
	"context"
	"errors"
)

// Verb is a canonical, API-oriented permission verb. The standardized set
// mirrors resource-oriented API operations; custom verbs are allowed as free
// strings (e.g. "download").
type Verb string

const (
	Get    Verb = "get"
	List   Verb = "list"
	Watch  Verb = "watch"
	Create Verb = "create"
	Update Verb = "update"
	Delete Verb = "delete"
)

// Principal is the authenticated caller. Tenant scoping is first-class and is
// taken from the verified token/context — never from a request body.
type Principal struct {
	Subject string
	Tenant  string
	Groups  []string
	Scopes  []string
	Claims  map[string]any
}

// Resource is the object acted upon. ID may be empty for collection verbs.
type Resource struct {
	Type string
	ID   string
}

// AccessRequest is the engine-neutral authorization question:
// "may Principal perform Verb on Resource?"
type AccessRequest struct {
	Principal Principal
	Verb      Verb
	Resource  Resource
	Method    string         // transport method (e.g. gRPC FullMethod), for logging/audit
	Context   map[string]any // extra attributes for attribute/relationship decisions
}

// Decision is the engine-neutral answer.
type Decision struct {
	Allow       bool
	Reason      string
	Obligations map[string]any // optional post-decision constraints (e.g. row filters)
}

// Authorizer is the pluggable decision point (PDP). It is the single seam that
// lets the enforcement engine change (dev → OPA → Cedar → remote) without
// touching service code.
type Authorizer interface {
	Authorize(ctx context.Context, req AccessRequest) (Decision, error)
}

// AuthorizerFunc adapts a function to an [Authorizer].
type AuthorizerFunc func(ctx context.Context, req AccessRequest) (Decision, error)

// Authorize implements [Authorizer].
func (f AuthorizerFunc) Authorize(ctx context.Context, req AccessRequest) (Decision, error) {
	return f(ctx, req)
}

// DenyAll is the safe default Authorizer: it denies everything. Used when no
// Authorizer is configured, so the system fails closed.
var DenyAll Authorizer = AuthorizerFunc(func(context.Context, AccessRequest) (Decision, error) {
	return Decision{Allow: false, Reason: "no authorizer configured (default deny)"}, nil
})

// ErrDenied is returned/queried by callers that prefer an error to a Decision.
var ErrDenied = errors.New("authz: permission denied")
