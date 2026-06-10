// Package authzpb extracts declared authorization rules from compiled protobuf
// descriptors. It reads the (infoblox.authz.v1.rule) method option — the
// "reflection over descriptors" approach (as protovalidate does for validation)
// — so a service's authz declarations live in its .proto and need no per-service
// generated rule file and no separate protoc plugin to run.
//
// The resulting []authz.MethodRule feeds both the gRPC interceptor (via
// grpcauthz.WithRules) and the permission catalog (authz/catalog) — one
// declaration, multiple consumers.
package authzpb

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/infobloxopen/devedge-sdk/authz"
	authzv1 "github.com/infobloxopen/apis/proto/infoblox/authz/v1"
)

// Rules walks every service method in files and returns the [authz.MethodRule]
// declared by the (infoblox.authz.v1.rule) option, keyed by gRPC FullMethod
// ("/pkg.Service/Method"). Methods without the option are omitted — turning a
// missing declaration into a hard error is the job of the caller's boot-time
// gate (grpcauthz.AssertMethodsDeclared).
func Rules(files *protoregistry.Files) []authz.MethodRule {
	var out []authz.MethodRule
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			methods := svc.Methods()
			for j := 0; j < methods.Len(); j++ {
				md := methods.Get(j)
				opts, ok := md.Options().(*descriptorpb.MethodOptions)
				if !ok || opts == nil || !proto.HasExtension(opts, authzv1.E_Rule) {
					continue
				}
				r, _ := proto.GetExtension(opts, authzv1.E_Rule).(*authzv1.Rule)
				if r == nil {
					continue
				}
				out = append(out, authz.MethodRule{
					Method:   fmt.Sprintf("/%s/%s", svc.FullName(), md.Name()),
					Verb:     authz.Verb(r.GetVerb()),
					Resource: r.GetResource(),
					Public:   r.GetPublic(),
				})
			}
		}
		return true
	})
	return out
}

// RulesFromGlobal extracts rules from every proto file linked into the binary
// (protoregistry.GlobalFiles). This is the typical call: a service's generated
// .pb.go registers itself globally on import.
func RulesFromGlobal() []authz.MethodRule { return Rules(protoregistry.GlobalFiles) }
