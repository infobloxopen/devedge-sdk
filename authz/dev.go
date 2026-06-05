package authz

import (
	"context"
	"slices"
	"strings"
)

// Grant is a development-time authorization grant: the listed Subjects may
// perform the listed Verbs on the given Resource type within a Tenant. It is a
// simple, readable structure for local development and tests — NOT a production
// policy format, and intentionally engine-neutral.
//
// Wildcards: Tenant and Resource may be "*"; Verbs may contain "*"; Subjects may
// contain "*" or "group:<name>".
type Grant struct {
	Tenant   string
	Subjects []string
	Verbs    []Verb
	Resource string
}

// DevAuthorizer is an in-process, default-deny [Authorizer] driven by a static
// set of [Grant]s. Suitable for local development and tests; not for production.
type DevAuthorizer struct {
	grants []Grant
}

// NewDevAuthorizer returns a default-deny Authorizer that allows a request iff
// some grant matches it.
func NewDevAuthorizer(grants ...Grant) *DevAuthorizer {
	return &DevAuthorizer{grants: grants}
}

// Authorize implements [Authorizer].
func (d *DevAuthorizer) Authorize(_ context.Context, req AccessRequest) (Decision, error) {
	for _, g := range d.grants {
		if g.matches(req) {
			return Decision{Allow: true, Reason: "dev grant matched"}, nil
		}
	}
	return Decision{Allow: false, Reason: "no dev grant matched (default deny)"}, nil
}

func (g Grant) matches(req AccessRequest) bool {
	if g.Tenant != "*" && g.Tenant != req.Principal.Tenant {
		return false
	}
	if !matchSubject(g.Subjects, req.Principal) {
		return false
	}
	if !matchVerb(g.Verbs, req.Verb) {
		return false
	}
	if g.Resource != "*" && g.Resource != req.Resource.Type {
		return false
	}
	return true
}

func matchSubject(subjects []string, p Principal) bool {
	for _, s := range subjects {
		if s == "*" || s == p.Subject {
			return true
		}
		if name, ok := strings.CutPrefix(s, "group:"); ok && slices.Contains(p.Groups, name) {
			return true
		}
	}
	return false
}

func matchVerb(verbs []Verb, v Verb) bool {
	return slices.Contains(verbs, v) || slices.Contains(verbs, Verb("*"))
}
