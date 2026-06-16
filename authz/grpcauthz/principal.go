package grpcauthz

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// DevPrincipalFunc returns a [PrincipalFunc] that derives an [authz.Principal]
// from incoming gRPC metadata, trusting it WITHOUT verification:
//
//   - "account-id" -> Principal.Tenant  (the same key middleware.TenantIDUnary reads)
//   - "subject"    -> Principal.Subject
//   - "groups"     -> Principal.Groups   (repeated metadata entries and/or a single
//     comma-separated value are both accepted)
//
// It exists so the documented development flow works end to end: a
// [authz.DevAuthorizer] grant such as {Tenant: "t1", Subjects: ["group:admin"]}
// can authorize a real request when the caller sends account-id: t1 and
// groups: admin.
//
// SECURITY: this trusts client-supplied headers as identity. Use it ONLY for
// local development and tests, paired with [authz.DevAuthorizer]. In production
// supply a PrincipalFunc that builds the Principal from a VERIFIED token (e.g. a
// validated JWT placed on the context by an authentication interceptor that runs
// before this one), never from raw request metadata.
func DevPrincipalFunc() PrincipalFunc {
	return func(ctx context.Context) (authz.Principal, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return authz.Principal{}, nil
		}
		p := authz.Principal{}
		if v := md.Get("account-id"); len(v) > 0 {
			p.Tenant = v[0]
		}
		if v := md.Get("subject"); len(v) > 0 {
			p.Subject = v[0]
		}
		for _, raw := range md.Get("groups") {
			for _, g := range strings.Split(raw, ",") {
				if g = strings.TrimSpace(g); g != "" {
					p.Groups = append(p.Groups, g)
				}
			}
		}
		return p, nil
	}
}
