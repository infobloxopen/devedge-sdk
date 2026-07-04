// Package storagegen holds engine-neutral helpers shared by the storage code
// generators (protoc-gen-ent and, per F027, protoc-gen-storage). Its job is to
// make the "can this resource field be deterministically wired into a
// persistence.Repository adapter?" decision IDENTICALLY across backends — so a
// resource behaves the same on ent and GORM, and a field the generator cannot
// map fails generation on both rather than being silently dropped (F027
// G-002 fail-closed, G-005 cross-backend parity).
package storagegen

// Field is the engine-neutral view of a proto resource field that the storage
// generators classify. Both plugins populate it from the same proto annotations
// (field.v1 options + field_behavior + descriptor kind), so classification does
// not depend on any ent- or GORM-specific type.
type Field struct {
	Name           string // proto field name (e.g. "fleet_id")
	IsID           bool   // the resource primary key
	IsTenant       bool   // account_id (supplied by the tenant mixin)
	IsSecret       bool   // secret field (stored hash+cipher)
	IsCredential   bool   // verify-only credential field (stored public_id + salted hash)
	IsTags         bool   // map<string,string> (stored as a JSON column)
	OutputOnly     bool   // AIP-203 OUTPUT_ONLY (surfaced on read, never written)
	IsRepeated     bool   // a repeated (list) field
	IsMessage      bool   // a nested message field (NOT a string map)
	IsEnum         bool   // an enum field
	IsRelationship bool   // carries a has_one/has_many/belongs_to/many_to_many annotation
	IsScalarFK     bool   // a scalar that is some relationship's foreign_key
	HasColumnType  bool   // maps to a recognized scalar column type (string/int/bool/bytes/…)
}

// Mappable reports whether a field can be deterministically wired into a
// repository adapter (its setter, projection, and — where relevant — filter
// column). A field that is not Mappable has no automatic representation and must
// be surfaced to the developer, not dropped.
//
// The order matters: a relationship is mappable even though it is a message or
// repeated field (it becomes an edge/association); only NON-relationship
// messages, repeated fields, and enums are unmappable.
func Mappable(f Field) bool {
	switch {
	case f.IsRelationship:
		return true // ent edge / GORM association
	case f.OutputOnly:
		// An OUTPUT_ONLY field is server-managed: never written from Create/Update
		// input. The generators back exactly two OUTPUT_ONLY shapes with real
		// storage: the framework fields (etag/delete_time/expire_time), which their
		// mixins own and which both plugins exclude upstream before classification,
		// and the AIP-122 resource `name`, which the projection derives from id (no
		// column). A plain OUTPUT_ONLY SCALAR outside that vocabulary has neither a
		// mixin nor a derivation, so it would get no column and no projection — every
		// write to it is silently discarded and every read returns the zero value.
		// Report it as unmapped so generation fails loudly (the developer drops
		// OUTPUT_ONLY to persist it as a server-writable column, or names it `name`
		// for the derived resource name) rather than shipping silent data loss.
		if f.Name == "name" {
			return true // derived AIP-122 resource name
		}
		if f.IsMessage || f.IsRepeated || f.IsEnum {
			return true // unstored computed field (e.g. an OUTPUT_ONLY Timestamp) — dropped as before
		}
		return false
	case f.IsMessage || f.IsRepeated || f.IsEnum:
		return false // nested non-relationship message, repeated scalar, or enum
	case f.IsID, f.IsTenant, f.IsSecret, f.IsCredential, f.IsTags, f.IsScalarFK:
		return true
	default:
		return f.HasColumnType // a plain scalar with a known column type
	}
}

// Classify partitions fields into those the generator can wire automatically and
// those it cannot. Callers must fail generation when unmapped is non-empty.
func Classify(fields []Field) (auto, unmapped []Field) {
	for _, f := range fields {
		if Mappable(f) {
			auto = append(auto, f)
		} else {
			unmapped = append(unmapped, f)
		}
	}
	return auto, unmapped
}

// Reason returns a human-actionable explanation (with the remedy) for why a
// field is unmapped. Only meaningful for fields where Mappable is false.
func Reason(f Field) string {
	switch {
	case f.OutputOnly:
		return "OUTPUT_ONLY field is server-managed, so the generated repository never writes it: outside the framework fields (etag/delete_time/expire_time) and the derived resource name, it gets no column and no projection, and every write to it is silently lost. To persist a server-computed value, remove OUTPUT_ONLY and set it in your handler (the field stays writable via a field mask); for the AIP-122 resource name, name the field \"name\""
	case f.IsMessage:
		return "nested message field has no scalar storage column — for a well-known type such as google.protobuf.Timestamp, model it as an int64 (unix seconds) column; otherwise add a relationship annotation (belongs_to/has_one/has_many) or flatten it into scalar fields"
	case f.IsRepeated:
		return "repeated field has no scalar storage column — model it as a has_many relationship or a separate resource"
	case f.IsEnum:
		return "enum field is not auto-wired — represent it as a string or int32 column"
	default:
		return "no recognized scalar storage column type"
	}
}
