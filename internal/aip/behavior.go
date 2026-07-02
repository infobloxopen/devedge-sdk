// Package aip is the shared, generator-agnostic resolver for the AIP contract
// facts devedge services declare in proto — field_behavior, AIP standard-method
// classification, and AIP-122 resource identity.
//
// It operates ONLY on protoreflect descriptors, the common denominator between
// the protogen types the protoc plugins use (protogen.Field.Desc,
// protogen.Method.Desc) and the FileDescriptorSet the OpenAPI enrichment pass
// reads (descriptorpb → protodesc.NewFiles → protoreflect). Because every
// generator AND the enrichment pass resolve these facts through this one package,
// a service's compiled behavior and its published OpenAPI cannot drift
// (WS-024 D-new-1).
//
// Dependencies are deliberately light: only google.golang.org/protobuf,
// the google.golang.org/genproto field_behavior/resource/resource_reference
// annotations, the private infoblox.field.v1 options, and the SDK-owned
// infoblox.ddd.v1 options — all already in the root module graph, so
// scripts/check-graph-isolation.sh stays green.
package aip

import (
	"fmt"

	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// FieldBehavior is the AIP google.api.field_behavior enum. Aliased so callers
// need not import the genproto annotations package directly.
type FieldBehavior = apiannotations.FieldBehavior

// Field behavior enum values used by this package and its callers.
const (
	Required   = apiannotations.FieldBehavior_REQUIRED
	OutputOnly = apiannotations.FieldBehavior_OUTPUT_ONLY
	InputOnly  = apiannotations.FieldBehavior_INPUT_ONLY
	Immutable  = apiannotations.FieldBehavior_IMMUTABLE
)

// ResolveFieldBehavior returns the EFFECTIVE field_behavior set for a proto
// field: the union of explicit (google.api.field_behavior) values and the values
// DERIVED from (infoblox.field.v1.opts), per the WS-024 D3 derivation table:
//
//   - secret = true                       → INPUT_ONLY (write-only, never returned)
//   - id.strategy STRATEGY_SERVER_GENERATED → OUTPUT_ONLY
//   - id.strategy STRATEGY_USER_SETTABLE    → IMMUTABLE
//
// not_null is NEVER mapped to REQUIRED — storage nullability is not an API
// contract of client-requiredness (a server-defaulted column is NOT NULL yet not
// client-required). REQUIRED is only ever an explicit
// (google.api.field_behavior) = REQUIRED on the field.
//
// It FAILS LOUD (returns an error naming the message, field, and conflicting
// behaviors) on a contradictory field: OUTPUT_ONLY combined with REQUIRED, or
// OUTPUT_ONLY combined with INPUT_ONLY — whether the conflict is between two
// explicit values, two derived values, or one of each. The returned slice is
// ordered deterministically (by enum number) so callers emit stable output.
func ResolveFieldBehavior(fd protoreflect.FieldDescriptor) ([]FieldBehavior, error) {
	set := map[FieldBehavior]bool{}
	for _, b := range explicitBehaviors(fd) {
		if b != apiannotations.FieldBehavior_FIELD_BEHAVIOR_UNSPECIFIED {
			set[b] = true
		}
	}
	for _, b := range derivedBehaviors(fd) {
		set[b] = true
	}

	if set[OutputOnly] && set[Required] {
		return nil, contradiction(fd, OutputOnly, Required)
	}
	if set[OutputOnly] && set[InputOnly] {
		return nil, contradiction(fd, OutputOnly, InputOnly)
	}

	// Deterministic order: iterate the enum values we care about in a fixed order.
	order := []FieldBehavior{
		apiannotations.FieldBehavior_OPTIONAL,
		Required,
		OutputOnly,
		InputOnly,
		Immutable,
		apiannotations.FieldBehavior_UNORDERED_LIST,
		apiannotations.FieldBehavior_NON_EMPTY_DEFAULT,
		apiannotations.FieldBehavior_IDENTIFIER,
	}
	var out []FieldBehavior
	for _, b := range order {
		if set[b] {
			out = append(out, b)
			delete(set, b)
		}
	}
	// Any behavior not in the fixed order (future-proofing) appended last, in
	// enum-number order via a stable pass over the remaining keys.
	for b := range set {
		out = append(out, b)
	}
	return out, nil
}

// IsOutputOnly reports whether a field resolves to OUTPUT_ONLY. It is the
// drop-in replacement for the three ad-hoc "== OUTPUT_ONLY" checks the codegen
// plugins carried; it propagates the fail-loud contradiction error.
func IsOutputOnly(fd protoreflect.FieldDescriptor) (bool, error) {
	bs, err := ResolveFieldBehavior(fd)
	if err != nil {
		return false, err
	}
	return HasBehavior(bs, OutputOnly), nil
}

// HasBehavior reports whether b is present in bs.
func HasBehavior(bs []FieldBehavior, b FieldBehavior) bool {
	for _, x := range bs {
		if x == b {
			return true
		}
	}
	return false
}

// AllowedValues returns the (infoblox.field.v1.opts).allowed_values constraint on
// a field. It is meaningful only for a singular string field (a string-backed
// enum), so non-string and repeated fields return nil. This maps to OpenAPI
// `enum`, not to a field_behavior.
func AllowedValues(fd protoreflect.FieldDescriptor) []string {
	if fd.Kind() != protoreflect.StringKind || fd.IsList() {
		return nil
	}
	fo := fieldOpts(fd)
	if fo == nil {
		return nil
	}
	return fo.GetAllowedValues()
}

// ReferenceTarget returns the AIP-124 cross-service reference target type
// declared on a field via the standard (google.api.resource_reference), and true
// when present. This is the WS-021 reference metadata the enriched OpenAPI
// surfaces as x-aip-references.
func ReferenceTarget(fd protoreflect.FieldDescriptor) (string, bool) {
	opts := fd.Options()
	if opts == nil || !proto.HasExtension(opts, apiannotations.E_ResourceReference) {
		return "", false
	}
	rr, _ := proto.GetExtension(opts, apiannotations.E_ResourceReference).(*apiannotations.ResourceReference)
	if rr == nil || rr.GetType() == "" {
		return "", false
	}
	return rr.GetType(), true
}

func explicitBehaviors(fd protoreflect.FieldDescriptor) []FieldBehavior {
	opts := fd.Options()
	if opts == nil || !proto.HasExtension(opts, apiannotations.E_FieldBehavior) {
		return nil
	}
	bs, _ := proto.GetExtension(opts, apiannotations.E_FieldBehavior).([]FieldBehavior)
	return bs
}

func derivedBehaviors(fd protoreflect.FieldDescriptor) []FieldBehavior {
	fo := fieldOpts(fd)
	if fo == nil {
		return nil
	}
	var out []FieldBehavior
	if fo.GetSecret() {
		out = append(out, InputOnly)
	}
	if id := fo.GetId(); id != nil {
		switch id.GetStrategy() {
		case fieldv1.IdOptions_STRATEGY_SERVER_GENERATED:
			out = append(out, OutputOnly)
		case fieldv1.IdOptions_STRATEGY_USER_SETTABLE:
			out = append(out, Immutable)
		}
	}
	// NOTE: not_null is intentionally NOT derived to REQUIRED (WS-024 D3).
	return out
}

func fieldOpts(fd protoreflect.FieldDescriptor) *fieldv1.FieldOptions {
	opts := fd.Options()
	if opts == nil || !proto.HasExtension(opts, fieldv1.E_Opts) {
		return nil
	}
	fo, _ := proto.GetExtension(opts, fieldv1.E_Opts).(*fieldv1.FieldOptions)
	return fo
}

func contradiction(fd protoreflect.FieldDescriptor, a, b FieldBehavior) error {
	return fmt.Errorf("aip: contradictory field_behavior on %s.%s: %s cannot combine with %s",
		fd.ContainingMessage().FullName(), fd.Name(), a, b)
}
