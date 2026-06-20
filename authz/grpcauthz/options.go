package grpcauthz

import (
	"context"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// PrincipalFunc extracts the authenticated [authz.Principal] from the request
// context (e.g. from a verified JWT placed there by an authentication
// interceptor that runs before this one).
type PrincipalFunc func(ctx context.Context) (authz.Principal, error)

// rule is the per-method authorization requirement.
type rule struct {
	verb     authz.Verb
	resource string
	public   bool
}

type config struct {
	authorizer  authz.Authorizer
	principalFn PrincipalFunc
	rules       map[string]rule // keyed by gRPC FullMethod; static rules from options
	// ruleSource, when set, supplies additional rules resolved lazily at
	// authorize-time (e.g. the server's accumulated AddRules set, which is not
	// known until every service has registered). It is merged over the static
	// rules: ruleSource wins on conflict.
	ruleSource func() []authz.MethodRule
	failOpen   bool
}

// lookup resolves the rule for fullMethod, consulting the lazy ruleSource (if
// any) before the static rule table so late-registered (AddRules) rules are
// visible to the interceptor at request time.
func (c *config) lookup(fullMethod string) (rule, bool) {
	if c.ruleSource != nil {
		for _, r := range c.ruleSource() {
			if r.Method == fullMethod {
				return rule{verb: r.Verb, resource: r.Resource, public: r.Public}, true
			}
		}
	}
	r, ok := c.rules[fullMethod]
	return r, ok
}

// Option configures the interceptor. The constructor + functional-option shape
// is intentionally rough-compatible with
// infobloxopen/atlas-authz-middleware/grpc_opa (see COMPAT.md).
type Option func(*config)

// WithAuthorizer sets the pluggable decision point. Defaults to [authz.DenyAll]
// (fail closed) if unset.
func WithAuthorizer(a authz.Authorizer) Option {
	return func(c *config) {
		if a != nil {
			c.authorizer = a
		}
	}
}

// WithPrincipalFunc sets how the Principal is extracted from context.
func WithPrincipalFunc(fn PrincipalFunc) Option {
	return func(c *config) {
		if fn != nil {
			c.principalFn = fn
		}
	}
}

// WithMethodRule declares the authz requirement for a gRPC FullMethod
// (e.g. "/dns.v1.ZoneService/GetZone"). In a generated setup these come from
// proto annotations; until then they are registered explicitly.
func WithMethodRule(fullMethod string, verb authz.Verb, resourceType string) Option {
	return func(c *config) {
		c.rules[fullMethod] = rule{verb: verb, resource: resourceType}
	}
}

// WithPublicMethod marks a FullMethod as not requiring authorization. This is
// the explicit, auditable opt-out; an unregistered method is denied.
func WithPublicMethod(fullMethod string) Option {
	return func(c *config) {
		c.rules[fullMethod] = rule{public: true}
	}
}

// WithRules registers a batch of declared [authz.MethodRule]s — e.g. the set
// produced from proto annotations. Equivalent to a WithMethodRule/WithPublicMethod
// per rule. The same []authz.MethodRule also feeds the permission catalog
// (authz/catalog), so a service declares its rules once and both the interceptor
// and the catalog stay in sync.
func WithRules(rules ...authz.MethodRule) Option {
	return func(c *config) {
		for _, r := range rules {
			c.rules[r.Method] = rule{verb: r.Verb, resource: r.Resource, public: r.Public}
		}
	}
}

// WithRuleSource supplies a function that returns the live set of declared
// rules, resolved lazily on each authorization (and by AssertMethodsDeclared).
// It lets the interceptor enforce over rules contributed AFTER construction —
// e.g. the server's accumulated AddRules set, which is only complete once every
// service has registered. Rules from the source take precedence over the static
// WithRules/WithMethodRule table on a method conflict.
func WithRuleSource(fn func() []authz.MethodRule) Option {
	return func(c *config) {
		if fn != nil {
			c.ruleSource = fn
		}
	}
}

// WithFailOpen disables fail-closed behavior. Strongly discouraged; provided
// only for migration/debugging.
func WithFailOpen(failOpen bool) Option {
	return func(c *config) { c.failOpen = failOpen }
}

func newConfig(opts ...Option) *config {
	c := &config{
		authorizer:  authz.DenyAll,
		principalFn: func(context.Context) (authz.Principal, error) { return authz.Principal{}, nil },
		rules:       map[string]rule{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}
