package authz

import (
	"context"
	"testing"
)

func TestDevAuthorizer(t *testing.T) {
	a := NewDevAuthorizer(
		Grant{Tenant: "t1", Subjects: []string{"u1"}, Verbs: []Verb{Get, List}, Resource: "zone"},
		Grant{Tenant: "t1", Subjects: []string{"group:admin"}, Verbs: []Verb{"*"}, Resource: "*"},
	)
	cases := []struct {
		name string
		req  AccessRequest
		want bool
	}{
		{"granted verb", AccessRequest{Principal: Principal{Subject: "u1", Tenant: "t1"}, Verb: Get, Resource: Resource{Type: "zone"}}, true},
		{"ungranted verb", AccessRequest{Principal: Principal{Subject: "u1", Tenant: "t1"}, Verb: Delete, Resource: Resource{Type: "zone"}}, false},
		{"cross-tenant denied", AccessRequest{Principal: Principal{Subject: "u1", Tenant: "t2"}, Verb: Get, Resource: Resource{Type: "zone"}}, false},
		{"group wildcard allows", AccessRequest{Principal: Principal{Subject: "u9", Tenant: "t1", Groups: []string{"admin"}}, Verb: Delete, Resource: Resource{Type: "anything"}}, true},
		{"no grant → default deny", AccessRequest{Principal: Principal{Subject: "nobody", Tenant: "t1"}, Verb: Get, Resource: Resource{Type: "zone"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := a.Authorize(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if dec.Allow != tc.want {
				t.Fatalf("Allow=%v want %v (%s)", dec.Allow, tc.want, dec.Reason)
			}
		})
	}
}
