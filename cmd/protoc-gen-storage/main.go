// Command protoc-gen-storage is a protoc/buf plugin that emits, for every
// proto message, a GORM-backed repository (.storage.go) implementing
// persistence.Repository[*pb.<Message>, string]:
//
//   - <Message>Model GORM struct with snake_case columns
//   - <Message>Repository with CRUD methods
//   - New<Message>Repository(*gorm.DB) constructor
//   - Compile-time persistence.Repository satisfaction check
//
// Generated code imports gorm.io/gorm; the consumer's go.mod provides GORM. The
// SDK's own clean core (top-level persistence, authz, grpcauthz) stays ORM-free —
// gorm.io/gorm is a dependency only of the sibling adapter persistence/gormtx,
// exactly as entgo.io/ent is confined to persistence/entrepo.
package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/infobloxopen/devedge-sdk/cmd/internal/storagegen"
	dddv1 "github.com/infobloxopen/devedge-sdk/proto/infoblox/ddd/v1"
	storagev1 "github.com/infobloxopen/apis/proto/infoblox/storage/v1"
	fieldv1 "github.com/infobloxopen/apis/proto/infoblox/field/v1"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	var flags flag.FlagSet
	// dialect selects the per-tenant-unique + soft-delete strategy (see
	// render.go targetDialect): "mysql" → a soft_delete_key discriminator column;
	// "postgres"/"sqlite" → a partial unique index (WHERE deleted_at IS NULL).
	dialect := flags.String("dialect", "postgres", "target SQL dialect: postgres|sqlite|mysql")
	protogen.Options{ParamFunc: flags.Set}.Run(func(gen *protogen.Plugin) error {
		targetDialect = *dialect
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		for _, f := range gen.Files {
			if f.Generate {
				generateFile(gen, f)
			}
		}
		return nil
	})
}

func generateFile(gen *protogen.Plugin, f *protogen.File) {
	var messages []messageInfo
	for _, m := range f.Messages {
		name := string(m.GoIdent.GoName)
		// Skip RPC request/response wrapper types — only resource messages get
		// a GORM model + Repository. Resource messages don't follow the
		// <Method>Request / <Method>Response naming convention.
		if strings.HasSuffix(name, "Request") || strings.HasSuffix(name, "Response") {
			continue
		}
		// Skip messages that have no "id" field — they are not storable resources
		// (e.g., AIP-151 operation-status and other value-object types).
		hasID := false
		for _, f := range m.Fields {
			if string(f.Desc.Name()) == "id" {
				hasID = true
				break
			}
		}
		if !hasID {
			continue
		}
		// F027 Phase 5b multi-surface: (infoblox.storage.v1.model) binds a message to
		// a backing storage model so several API surfaces can project ONE table.
		// Resolve the model name now (absent/empty => the message's own name, the
		// normal single-surface case). A surface (model != name) emits a repository
		// adapter + projection over its owner's GORM type but NO table of its own.
		resolvedModel := name
		if opts := m.Desc.Options(); opts != nil && proto.HasExtension(opts, storagev1.E_Model) {
			if mv, _ := proto.GetExtension(opts, storagev1.E_Model).(string); mv != "" {
				resolvedModel = mv
			}
		}
		msg := messageInfo{
			MessageName:  name,
			Model:        resolvedModel,
			PbPkgName:    string(f.GoPackageName),
			PbImportPath: string(f.GoImportPath),
		}
		// F031 DDD: read the SDK-owned infoblox.ddd.v1 message options (mirror
		// protoc-gen-ent main.go). (aggregate) marks an aggregate ROOT; (member)
		// marks a resource OWNED by a named root. A member→root containment drives
		// OnDelete: CASCADE on the owning GORM edge, and a root with containment
		// members gets a Load<Root>Aggregate graph-load primitive (Phase 2).
		if mopts := m.Desc.Options(); mopts != nil {
			if proto.HasExtension(mopts, dddv1.E_Aggregate) {
				if ag, ok := proto.GetExtension(mopts, dddv1.E_Aggregate).(*dddv1.Aggregate); ok && ag != nil {
					msg.AggregateRoot = ag.GetRoot()
				}
			}
			if proto.HasExtension(mopts, dddv1.E_Member) {
				if mb, ok := proto.GetExtension(mopts, dddv1.E_Member).(*dddv1.Member); ok && mb != nil {
					msg.MemberRoot = mb.GetRoot()
				}
			}
		}
		// Extract (google.api.resource) pattern from message options.
		if mopts := m.Desc.Options(); mopts != nil {
			if proto.HasExtension(mopts, apiannotations.E_Resource) {
				if rd, ok := proto.GetExtension(mopts, apiannotations.E_Resource).(*apiannotations.ResourceDescriptor); ok {
					if patterns := rd.GetPattern(); len(patterns) > 0 {
						msg.ResourcePattern = patterns[0]
					}
				}
			}
		}
		for _, field := range m.Fields {
			var (
				isSecret     bool
				isOutputOnly bool
				notNull      bool
				unique       bool
				index        bool
				columnName   string
				columnType   string
				hasOne       *fieldv1.HasOne
				hasMany      *fieldv1.HasMany
				belongsTo    *fieldv1.BelongsTo
				manyToMany   *fieldv1.ManyToMany
				references   *dddv1.References
			)
			if opts := field.Desc.Options(); opts != nil {
				if proto.HasExtension(opts, fieldv1.E_Opts) {
					if fopts, ok := proto.GetExtension(opts, fieldv1.E_Opts).(*fieldv1.FieldOptions); ok {
						isSecret = fopts.GetSecret()
						notNull = fopts.GetNotNull()
						unique = fopts.GetUnique()
						index = fopts.GetIndex()
						columnName = fopts.GetColumnName()
						columnType = fopts.GetColumnType()
						hasOne = fopts.GetHasOne()
						hasMany = fopts.GetHasMany()
						belongsTo = fopts.GetBelongsTo()
						manyToMany = fopts.GetManyToMany()
					}
				}
				// F031 DDD: (infoblox.ddd.v1.references) is a CROSS-aggregate link (scalar
				// FK + ID, no association) — distinct from the WITHIN-aggregate containment
				// edges that drive cascade/graph-load. Like ent, the references field is
				// dropped from the model (only the sibling scalar FK column persists), so
				// events reference the other aggregate by id only. Reading it here marks
				// the field as relationship-mapped for the fail-closed coverage check.
				if proto.HasExtension(opts, dddv1.E_References) {
					if rf, ok := proto.GetExtension(opts, dddv1.E_References).(*dddv1.References); ok && rf != nil {
						references = rf
					}
				}
				if proto.HasExtension(opts, apiannotations.E_FieldBehavior) {
					behaviors, _ := proto.GetExtension(opts, apiannotations.E_FieldBehavior).([]apiannotations.FieldBehavior)
					for _, b := range behaviors {
						if b == apiannotations.FieldBehavior_OUTPUT_ONLY {
							isOutputOnly = true
						}
					}
				}
			}
			// AIP-148: detect soft-delete and TTL markers. These are handled specially
			// by the renderer and must NOT be added to msg.Fields as ordinary columns.
			fieldName := string(field.Desc.Name())
			isTimestamp := field.Desc.Kind() == protoreflect.MessageKind &&
				field.Desc.Message() != nil &&
				field.Desc.Message().FullName() == "google.protobuf.Timestamp"
			if isOutputOnly && isTimestamp {
				if fieldName == "delete_time" {
					msg.SoftDelete = true
					continue // handled by renderer; not an ordinary column
				}
				if fieldName == "expire_time" {
					msg.HasExpireTime = true
					continue // handled by renderer; not an ordinary column
				}
			}
			// AIP-154 ETag: a string `etag` field is framework-managed. The model
			// already carries an `etag` column unconditionally; the renderer stamps
			// it on every write and surfaces it on read. Skip it as an ordinary
			// column (emitting it would duplicate the `etag` column).
			if fieldName == "etag" && field.Desc.Kind() == protoreflect.StringKind {
				msg.HasETag = true
				continue
			}
			// For message-kind fields (relationships), capture the related message's
			// Go type name so the renderer can emit a concrete GORM association
			// (*<Related>Model) instead of an unusable interface{}.
			relatedGoType := ""
			if field.Message != nil {
				relatedGoType = string(field.Message.GoIdent.GoName)
			}
			// A proto map<string, string> is the Tags field kind: persisted as a
			// single JSONB column (types.Tags), not a nested message or relation. A
			// map arrives as Kind()==MessageKind with IsMap()==true and a synthetic
			// map-entry message, so it must be detected here or it falls into the
			// nested-message skip below. Non-string maps keep the old skip behavior.
			isStringMap := field.Desc.IsMap() &&
				field.Desc.MapKey().Kind() == protoreflect.StringKind &&
				field.Desc.MapValue().Kind() == protoreflect.StringKind
			msg.Fields = append(msg.Fields, fieldInfo{
				Name:          string(field.Desc.Name()),
				GoFieldName:   string(field.GoName),
				SnakeName:     toSnake(string(field.Desc.Name())),
				IsRepeated:    field.Desc.IsList(),
				IsMessage:     field.Desc.Kind() == protoreflect.MessageKind && !isStringMap,
				IsEnum:        field.Desc.Kind() == protoreflect.EnumKind,
				IsTags:        isStringMap,
				IsID:          string(field.Desc.Name()) == "id",
				GoType:        protoKindToGoType(field.Desc.Kind()),
				RelatedGoType: relatedGoType,
				IsSecret:      isSecret,
				IsOutputOnly:  isOutputOnly,
				NotNull:       notNull,
				Unique:        unique,
				Index:         index,
				ColumnName:    columnName,
				ColumnType:    columnType,
				HasOne:        hasOne,
				HasMany:       hasMany,
				BelongsTo:     belongsTo,
				ManyToMany:    manyToMany,
				References:    references,
			})
		}
		messages = append(messages, msg)
	}

	if len(messages) == 0 {
		return
	}

	// F027 fail-closed (G-002): every resource field must be deterministically
	// wirable into the generated repository adapter. A field with no mapping — a
	// nested non-relationship message, a repeated non-relationship field, an enum,
	// a non-string map — would otherwise be silently dropped. Fail generation
	// instead, naming the field and the remedy, using the engine-neutral classifier
	// shared with protoc-gen-ent (G-005).
	failed := false
	for _, msg := range messages {
		_, unmapped := storagegen.Classify(toStorageFields(msg))
		for _, uf := range unmapped {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: %s", msg.MessageName, uf.Name, storagegen.Reason(uf)))
			failed = true
		}
	}
	if failed {
		return
	}

	// F027 Phase 5b multi-surface: index every message by name so a surface can be
	// matched to its owner, then validate the surfaces fail-closed.
	ownerByName := make(map[string]messageInfo, len(messages))
	for _, msg := range messages {
		ownerByName[msg.MessageName] = msg
	}
	if !validateSurfaces(gen, messages, ownerByName) {
		return
	}

	// Storage code lives in the same package as the pb types (same directory).
	// This keeps the generated file co-located with widgets.pb.go so the GORM
	// model can reference proto types without a package qualifier.
	// Pass an empty PbPkgName to renderStorageFile to skip the qualifier.
	for i := range messages {
		messages[i].PbPkgName = "" // same package — no qualifier needed
	}
	content := renderStorageFile(string(f.GoPackageName), messages, ownerByName)
	if content == "" {
		return
	}

	outPath := f.GeneratedFilenamePrefix + ".storage.go"
	g := gen.NewGeneratedFile(outPath, f.GoImportPath)
	g.P(content)
}

// validateSurfaces enforces the F027 Phase 5b multi-surface contract fail-closed.
// A SURFACE message (one whose (infoblox.storage.v1.model) names a different
// message) projects an OWNER's table, so the generated adapter writes/reads the
// owner's GORM columns. Returns true when all surfaces are valid.
func validateSurfaces(gen *protogen.Plugin, messages []messageInfo, ownerByName map[string]messageInfo) bool {
	ok := true
	for _, msg := range messages {
		if !msg.isSurface() {
			continue
		}
		owner, found := ownerByName[msg.Model]
		if !found {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: (infoblox.storage.v1.model)=%q names a model with no message in this proto; declare a message %s or correct the annotation", msg.MessageName, msg.Model, msg.Model))
			ok = false
			continue
		}
		if owner.isSurface() {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: (infoblox.storage.v1.model)=%q points at another surface; a surface must name a base model", msg.MessageName, msg.Model))
			ok = false
			continue
		}
		ownerFields := make(map[string]fieldInfo, len(owner.Fields))
		for _, of := range owner.Fields {
			ownerFields[of.SnakeName] = of
		}
		if msgHasTenantField(owner) && !msgHasTenantField(msg) {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: a surface of the tenant-scoped model %s must include the account_id field so the adapter can scope and stamp the tenant", msg.MessageName, msg.Model))
			ok = false
		}
		for _, f := range msg.Fields {
			if f.IsID {
				continue // every model has the id primary key
			}
			if f.IsRepeated || f.IsMessage {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: relationships/nested messages are not supported on a model surface; declare them on the model %s", msg.MessageName, f.Name, msg.Model))
				ok = false
				continue
			}
			of, has := ownerFields[f.SnakeName]
			if !has {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s: a surface must project a subset of model %s's fields, but %s has no field %q", msg.MessageName, f.Name, msg.Model, msg.Model, f.SnakeName))
				ok = false
				continue
			}
			if f.GoType != of.GoType || f.IsSecret != of.IsSecret || f.IsOutputOnly != of.IsOutputOnly {
				gen.Error(fmt.Errorf("protoc-gen-storage: %s.%s conflicts with model %s.%s: a surface field must match the model column's type and secret/output classification", msg.MessageName, f.Name, msg.Model, of.Name))
				ok = false
			}
		}
		// Framework parity checks: surface flags must be compatible with owner.
		if msg.SoftDelete && !owner.SoftDelete {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: surface declares soft-delete but owner %s does not", msg.MessageName, msg.Model))
			ok = false
		}
		if msg.HasExpireTime && !owner.HasExpireTime {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: surface declares expire_time but owner %s does not", msg.MessageName, msg.Model))
			ok = false
		}
		if msg.HasETag && !owner.HasETag {
			gen.Error(fmt.Errorf("protoc-gen-storage: %s: surface declares etag but owner %s does not", msg.MessageName, msg.Model))
			ok = false
		}
	}
	return ok
}

func protoKindToGoType(k protoreflect.Kind) string {
	switch k {
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.FloatKind:
		return "float32"
	case protoreflect.DoubleKind:
		return "float64"
	case protoreflect.BytesKind:
		return "[]byte"
	default:
		return "interface{}" // enum, message — caller checks IsMessage separately
	}
}

// toSnake converts camelCase or snake_case to snake_case.
func toSnake(s string) string {
	// proto field names are already snake_case by convention.
	return strings.ToLower(s)
}
