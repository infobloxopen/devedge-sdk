// Package apilayout is the URL layout strategy seam: it turns a resource's
// (domain, version, resource) coordinates into a REST path according to a
// chosen, named strategy. The default strategy is product-friendly REST.
//
// A layout is a mechanism, not a policy: the same coordinates render different
// paths under different strategies, and callers (the service scaffold, the edge
// shell, docs) select a strategy without knowing how any other renders.
//
//	product-rest (default): /api/{domain}/{version}/{resource}[/{id}]
//	                         /api/ipam/v1/ip-spaces/prod
//	k8s-apis:                /apis/{group}/{version}/{resource}[/{name}]
//	                         /apis/ipam.infoblox.com/v1/ip-spaces/prod
//
// Naming rules the seam assumes (the industry and org convention):
//
//	domain   short product domain, e.g. "ipam", "dns"       (product-rest)
//	group    fully-qualified API group, e.g. "ipam.infoblox.com" (k8s-apis)
//	version  "v1", "v1beta1", "v2"                            (precedes the resource)
//	resource plural, lower-kebab collection name, e.g. "ip-spaces"
//
// Version always precedes the resource: it describes the contract used to
// interpret everything after it. Rendering "version after resource" is not a
// strategy this package offers.
package apilayout

import (
	"fmt"
	"regexp"
	"strings"
)

// Layout names a URL layout strategy.
type Layout string

const (
	// ProductREST renders /api/{domain}/{version}/{resource}. Readable public
	// URLs; each domain evolves its major version independently. The default.
	ProductREST Layout = "product-rest"

	// K8sAPIs renders /apis/{group}/{version}/{resource}, the Kubernetes API
	// group/version/resource shape. Strong for platform/discovery and
	// declarative surfaces.
	K8sAPIs Layout = "k8s-apis"
)

// Default is the layout used when none is configured.
const Default = ProductREST

// All returns every supported layout, in a stable order.
func All() []Layout { return []Layout{ProductREST, K8sAPIs} }

// Parse validates s and returns the corresponding Layout. An empty string
// resolves to Default.
func Parse(s string) (Layout, error) {
	switch Layout(s) {
	case "":
		return Default, nil
	case ProductREST:
		return ProductREST, nil
	case K8sAPIs:
		return K8sAPIs, nil
	default:
		return "", fmt.Errorf("apilayout: unknown layout %q (want one of %v)", s, All())
	}
}

// Prefix returns the layout's top-level path segment: "/api" for product-rest,
// "/apis" for k8s-apis.
func (l Layout) Prefix() string {
	if l == K8sAPIs {
		return "/apis"
	}
	return "/api"
}

// Resource is a resource's coordinates. product-rest uses Domain; k8s-apis uses
// Group. Version and Resource are used by every layout.
type Resource struct {
	Domain   string // short product domain (product-rest), e.g. "ipam"
	Group    string // fully-qualified API group (k8s-apis), e.g. "ipam.infoblox.com"
	Version  string // "v1", "v1beta1", "v2"
	Resource string // plural collection, e.g. "ip-spaces"
}

var (
	versionRE = regexp.MustCompile(`^v[0-9]+((alpha|beta)[0-9]+)?$`)
	segmentRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	groupRE   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*(\.[a-z0-9]+(-[a-z0-9]+)*)+$`)
)

// domainSegment returns the domain/group segment for the layout and validates it.
func (l Layout) domainSegment(r Resource) (string, error) {
	if l == K8sAPIs {
		if r.Group == "" {
			return "", fmt.Errorf("apilayout: k8s-apis requires a Group (e.g. %q)", "ipam.infoblox.com")
		}
		if !groupRE.MatchString(r.Group) {
			return "", fmt.Errorf("apilayout: invalid group %q (want a dotted name like ipam.infoblox.com)", r.Group)
		}
		return r.Group, nil
	}
	if r.Domain == "" {
		return "", fmt.Errorf("apilayout: product-rest requires a Domain (e.g. %q)", "ipam")
	}
	if !segmentRE.MatchString(r.Domain) {
		return "", fmt.Errorf("apilayout: invalid domain %q (want a lower-kebab name like ipam)", r.Domain)
	}
	return r.Domain, nil
}

// validate checks the version and resource segments, common to every layout.
func (r Resource) validate() error {
	if !versionRE.MatchString(r.Version) {
		return fmt.Errorf("apilayout: invalid version %q (want v1, v1beta1, v2, …)", r.Version)
	}
	if !segmentRE.MatchString(r.Resource) {
		return fmt.Errorf("apilayout: invalid resource %q (want a lower-kebab plural like ip-spaces)", r.Resource)
	}
	return nil
}

// CollectionPath renders the collection path, e.g. /api/ipam/v1/ip-spaces.
func (l Layout) CollectionPath(r Resource) (string, error) {
	dom, err := l.domainSegment(r)
	if err != nil {
		return "", err
	}
	if err := r.validate(); err != nil {
		return "", err
	}
	return strings.Join([]string{l.Prefix(), dom, r.Version, r.Resource}, "/"), nil
}

// ItemPath renders the item path with a trailing id/name parameter, e.g.
// /api/ipam/v1/ip-spaces/{id}. idParam is used verbatim (pass "{id}" for a
// proto google.api.http template, or a concrete id for a live URL).
func (l Layout) ItemPath(r Resource, idParam string) (string, error) {
	if idParam == "" {
		return "", fmt.Errorf("apilayout: ItemPath requires a non-empty idParam")
	}
	base, err := l.CollectionPath(r)
	if err != nil {
		return "", err
	}
	return base + "/" + idParam, nil
}
