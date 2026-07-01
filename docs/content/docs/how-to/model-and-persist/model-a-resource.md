---
title: Model a Resource
weight: 2
aliases:
  - /docs/guides/model-a-resource/
  - /docs/guides/secret-fields/
---

A resource is a proto message that the storage generators (`protoc-gen-storage`, `protoc-gen-ent`)
turn into a persisted database table, a repository, and a set of lifecycle helpers. Declaring
fields with the right types and annotations gives you tenant isolation, concurrency control,
soft-delete, secret handling, and tag filtering without writing the plumbing yourself.

Use this guide when you are designing the proto message for a new resource, choosing which field
types to use, or wiring storage constraints and relationships. [Define a Service](../define-a-service/)
covers the generate-and-wire loop that surrounds this message.

## Resource recognition

The storage generators produce a `<Name>Model` / ent schema and a `<Name>Repository` for a message
when both conditions are true:

1. The message has a string **`id` field**, which becomes the primary key (`varchar(36)`).
2. The message name does not end in `Request` or `Response` — those RPC wrappers are skipped.

All other messages (value objects, request/response envelopes without an `id`) are left untouched.
The smallest valid resource is:

```proto
message Widget {
  string id           = 1;
  string display_name = 2;
}
```

That definition generates a `WidgetModel`, a CRUDL `WidgetRepository`, and the columns `id` and
`display_name`, plus the framework's automatic `created_at`, `updated_at`, and `etag` columns.

## Resource name (AIP-122)

Optionally attach `(google.api.resource)` to mark the resource type and its name pattern. The generator then
emits a `name` value, a `<Name>NamePattern` constant, and `Format<Name>Name` /
`Parse<Name>Name` helpers (backed by [`persistence/resourcename`](../../../reference/persistence/)):

```proto
message Widget {
  option (google.api.resource) = {
    type: "toy.example.com/Widget"
    pattern: "widgets/{widget}"
  };

  // name is the AIP-122 resource name (e.g. "widgets/abc123"), computed from id.
  string name = 1 [(google.api.field_behavior) = OUTPUT_ONLY];
  string id   = 2;
  // ...
}
```

`name` is `OUTPUT_ONLY` (see [Framework-managed fields](#framework-managed-fields)) — it is derived
from `id` and is never stored or accepted on input. Both storage backends enforce this: GORM omits
the column and populates `name` in `fromModel`; ent omits the column and recomputes it in the
generated `fromEnt<R>` projection (see
[codegen → Resource names on ent](../../../reference/codegen/#resource-names-on-ent-aip-122)). A
read always carries the resource name with no consumer code.

## Field types

Scalar proto types map to columns as follows:

| Proto type | Column (Go) |
|---|---|
| `string` | `string` (`varchar`) |
| `bool` | `bool` |
| `int32` / `sint32` / `sfixed32` | `int32` |
| `int64` / `sint64` / `sfixed64` | `int64` |
| `uint32` / `fixed32` | `uint32` |
| `uint64` / `fixed64` | `uint64` |
| `float` | `float32` |
| `double` | `float64` |
| `bytes` | `[]byte` (BLOB) |

{{< callout type="warning" >}}
**Not every proto type maps to a column.** Repeated scalars and nested messages without a
[relationship annotation](#relationships) are skipped — they require a JSON/JSONB serialization
strategy that the generator does not yet produce. The one exception is `map<string, string>`, which
persists as a JSONB column (see [Tags](#tags)). Enums do not map to a typed column; represent an
enum as a `string` field and constrain it with `allowed_values` (see
[Constraints and column overrides](#constraints-and-column-overrides)), or as an integer field.
`google.protobuf.Timestamp`
carries built-in storage meaning only for the two [framework fields](#framework-managed-fields)
described below; other timestamp fields are not persisted by the generator.
{{< /callout >}}

## Framework-managed fields

Certain well-known field names receive automatic storage behavior. Declare them in the proto and the
framework wires the storage and lifecycle; no additional plumbing is required.

| Field (proto) | Type | Behavior |
|---|---|---|
| `account_id` | `string` | **Tenant scope.** Every query is automatically filtered by `account_id` from `middleware.TenantIDFromContext(ctx)` — see [Tenant Isolation](../../../concepts/tenant-isolation/). Also makes `unique` constraints per-tenant (see [Constraints and column overrides](#constraints-and-column-overrides)). |
| `etag` | `string` `OUTPUT_ONLY` | **AIP-154 concurrency control.** Stamped on every write and surfaced on read; the client echoes it as `If-Match` for a 412-guarded conditional update. Auto-stamped on both backends with no consumer code — ent uses the generated `EtagMixin` plus the generated `fromEnt` projection — see [codegen → ETag](../../../reference/codegen/). |
| `delete_time` | `Timestamp` `OUTPUT_ONLY` | **AIP-148 soft-delete.** Opts the resource into soft-delete (a `DeletedAt` column). `Delete` sets it, `Undelete` clears it, and `List` hides soft-deleted rows unless `show_deleted` is set. Omit the field for hard-delete. A per-tenant `unique` key on a soft-delete resource is re-creatable once the holder is soft-deleted — see [codegen → Soft-delete + unique](../../../reference/codegen/) (`dialect=mysql` for the MySQL strategy). |
| `expire_time` | `Timestamp` `OUTPUT_ONLY` | **AIP-148 TTL.** Adds an `expire_time` column and a `PurgeExpired` method to the repository. |
| `created_at`, `updated_at` | — | Added to every model automatically; you do not declare them. |

`OUTPUT_ONLY` (from `google.api.field_behavior`) marks a field as server-managed. The field is
returned to clients but is never read from a create or update request, so it is never persisted from
input. The `APIKey` resource below uses four of these at once:

```proto {filename="apikey.proto"}
message APIKey {
  option (google.api.resource) = {type: "apikey.example.com/APIKey" pattern: "apikeys/{api_key}"};

  string name        = 1 [(google.api.field_behavior) = OUTPUT_ONLY]; // resource name
  string id          = 2;
  string account_id  = 3;                                              // tenant scope
  string key_value   = 4 [(infoblox.field.v1.opts) = {secret: true}]; // see Secret fields
  string key_prefix  = 5;
  string label       = 6;
  google.protobuf.Timestamp delete_time = 7 [(google.api.field_behavior) = OUTPUT_ONLY]; // soft-delete
  google.protobuf.Timestamp expire_time = 8 [(google.api.field_behavior) = OUTPUT_ONLY]; // TTL
  string etag        = 9 [(google.api.field_behavior) = OUTPUT_ONLY];                    // concurrency
}
```

## Constraints and column overrides

`(infoblox.field.v1.opts)` carries per-field storage constraints that the generators translate into
the model:

| Option | Effect |
|---|---|
| `not_null` | `NOT NULL` on the column |
| `unique` | Unique index — per-tenant (composite with `account_id`) when the message has an `account_id` field, otherwise global |
| `unique_with` | Extends a `unique` field's per-tenant composite with sibling columns — "unique within a parent" (see [Unique within a parent](#unique-within-a-parent)) |
| `index` | Non-unique index |
| `allowed_values` | Restricts a string field to a fixed set of values, validated on create and update (see [Validated string enums](#validated-string-enums)) |
| `column_name` | Overrides the default snake_case column name |
| `column_type` | Overrides the DB column type, e.g. `varchar(255)` |

```proto
message Widget {
  string id    = 1;
  string slug  = 2 [(infoblox.field.v1.opts) = {unique: true, not_null: true}];
  string notes = 3 [(infoblox.field.v1.opts) = {column_type: "text"}];
}
```

{{< callout type="info" >}}
**`unique` is per-tenant by default.** In a message that has `account_id`, a `unique` field joins
`account_id` in a composite index (with `account_id` leading), so one tenant's values do not
collide with another's and do not leak across tenants. See the
[Annotations concept](../../../concepts/annotations/) for the full option reference and the
rationale.
{{< /callout >}}

### Unique within a parent

To make a field unique within a parent rather than across the whole tenant, list the parent's
foreign-key field in `unique_with`. The generator inserts those columns into the composite unique
index, between the tenant scope and the field:

```proto
message CartItem {
  string id         = 1;
  string account_id = 2;
  string cart_id    = 3;                                                    // the parent FK
  string sku        = 4 [(infoblox.field.v1.opts) = {unique: true, unique_with: ["cart_id"]}];
}
```

This yields a unique index over `(account_id, cart_id, sku)` on both backends: the same `sku` can
appear in different carts, but not twice in one cart. `unique_with` requires `unique: true` and an
`account_id` field, and each name must be a scalar field on the same message. On a soft-delete
resource the composite stays re-creatable after the holder is soft-deleted, using the same
partial-index or discriminator strategy as a plain per-tenant unique.

### Validated string enums

A string field with a fixed set of values — a state field such as `status` — carries an
`allowed_values` list. The generated Create and Update handlers reject a value outside the set with
`InvalidArgument` before it reaches the database:

```proto
message Cart {
  string id     = 1;
  string status = 2 [(infoblox.field.v1.opts) = {allowed_values: ["ACTIVE", "CHECKED_OUT", "ABANDONED"]}];
}
```

An empty value is treated as unset and is not checked; combine with `not_null` to require a value.

## Relationships

To model how resources connect, annotate a message-typed or repeated message-typed field. The
generators emit the matching GORM association and ent edge:

| Option | Declare on | Cardinality / FK |
|---|---|---|
| `has_one` | a message field | 1:1, FK on the *other* table |
| `has_many` | a repeated message field | 1:N, FK on the *other* table |
| `belongs_to` | a message field | the inverse — FK on *this* table |
| `many_to_many` | a repeated message field | N:N via a join table |

The AIP-standard pattern for a parent/child pair is to expose the foreign key as a scalar field
(so clients can read and filter it) and also annotate the association. The `fleet` fixture below
shows a `Fleet` that has many `Vehicle`s; each `Vehicle` belongs to one `Fleet` via a scalar
`fleet_id`:

```proto {filename="fleet.proto"}
message Fleet {
  string id           = 1;
  string account_id   = 2;
  string display_name = 3 [(infoblox.field.v1.opts) = {unique: true}]; // unique per tenant
  // 1:N — the child Vehicles, keyed by the fleet_id FK on the Vehicle side.
  repeated Vehicle vehicles = 4 [(infoblox.field.v1.opts) = {has_many: {foreign_key: "fleet_id"}}];
}

message Vehicle {
  string id         = 1;
  string account_id = 2;
  string vin        = 3 [(infoblox.field.v1.opts) = {unique: true}];
  string fleet_id   = 4;                                              // scalar FK, queryable per AIP
  // The inverse: FK (fleet_id) lives on this table. Reuses the scalar above — no duplicate column.
  Fleet  fleet      = 5 [(infoblox.field.v1.opts) = {belongs_to: {foreign_key: "fleet_id"}}];
}
```

The related message must itself be a resource (it needs an `id`). On the GORM backend these become
typed associations (`[]*VehicleModel`, `*FleetModel`) you can `Preload`; on the ent backend they
become graph edges, which suits relationship-heavy domains. See
[Storage Shapes](../storage-shapes/) for when to choose ent, and the
[Codegen reference](../../../reference/codegen/) for the exact generated shape.

{{< callout type="info" >}}
**A `has_many` is not eager-loaded on a plain read.** `Get<Parent>` returns the parent with an empty
children slice, which keeps a single-resource read from loading an unbounded set of children. To read
a parent together with its children, use the generated `Load<Parent>Aggregate` read primitive — see
[Aggregates → Loading and saving](../../../concepts/aggregates/#loading-and-saving).
{{< /callout >}}

## Secret fields

A field annotated with `secret` stores a value that must never appear as plaintext in the database
or in any response after creation. The storage generator, logging middleware, and security gate all
enforce this together.

Declare the annotation in the proto:

```proto
string key_value = 4 [(infoblox.field.v1.opts) = {secret: true}];
```

### Storage

`protoc-gen-storage` does not emit a plaintext column for a secret field. It emits two columns
instead — a deterministic **hash** (for lookup) and a **cipher** (for recovery) — and omits the
plaintext from the model entirely:

```go
type APIKeyModel struct {
    ID             string `gorm:"primaryKey;type:varchar(36)"`
    // ...
    KeyValueHash   string `gorm:"column:key_value_hash;index"` // for lookup
    KeyValueCipher string `gorm:"column:key_value_cipher"`      // for recovery
    // ...
}
```

The `toModel`/`fromModel` helpers skip secret fields, so plaintext cannot round-trip through the
model. Encryption happens in `Create`/`Update`. Because the repository requires an `Encryptor`,
the generated constructor accepts one:

```go
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
```

To look up a record by its raw value, use the generated `LookupBy<Field>Hash` method, which is
tenant-scoped:

```go
h, _ := enc.Hash(ctx, presentedKey)
key, err := repo.LookupByKeyValueHash(ctx, h)
```

### Encryptor implementations

Both implementations satisfy one interface (`Encrypt` / `Decrypt` / `Hash`):

- **Dev** — `secret.NewDev(key)` uses AES-256-GCM + HMAC-SHA256, all in-process (the key must be
  ≥ 32 bytes). Use this for local development and tests.
- **Production** — `secret.NewVaultTransit(addr, token, keyName)` calls HashiCorp Vault's Transit
  engine over plain HTTP (no Vault SDK dependency). See [Secret Fields → Vault Transit backend](../../secure/secret-fields/#vault-transit-backend-production).

{{< callout type="warning" >}}
**Do not use `secret.NewDev` in production.** It performs encryption in-process; use
`secret.NewVaultTransit` for any deployed environment.
{{< /callout >}}

The full `Encryptor` API is in the [secret reference](../../../reference/secret/).

### Redaction and leak checks

The `secret` annotation also affects logging and response validation:

- **Logs** — `middleware/redact` replaces the value with `[REDACTED]` before logging.
- **Responses** — `seccheck.AssertNoSecretFieldsLeaked(resp...)` walks every response proto and
  fails if a secret field holds anything other than `[REDACTED]`. Wire it into your tests — see
  [Security Check](../../secure/security-check/).

{{< callout type="warning" >}}
**Your handler receives the raw value on the request.** Return it to the caller at most once, at
creation time — never on `Get` or `List`. `AssertNoSecretFieldsLeaked` is the safety net for this
constraint.
{{< /callout >}}

## Tags

A `map<string, string>` field is treated as a tags field: a free-form set of string labels
persisted as a single JSONB column. No annotation is needed — the storage generators detect a
`map<string, string>` and wire it automatically.

```proto
message APIKey {
  string id = 2;
  // ...
  map<string, string> tags = 10;
}
```

The two backends handle the column differently:

- **GORM** (`protoc-gen-storage`) emits a `types.Tags` column tagged `gorm:"type:jsonb"`.
  `github.com/infobloxopen/devedge-sdk/types.Tags` is an ORM-agnostic `map[string]string` that
  implements `driver.Valuer` / `sql.Scanner` (the atlas custom-field-type convention), so it
  persists transparently. `jsonb` is native on Postgres and works on SQLite via type affinity. The
  type ships helpers (`Merge`, `Clone`, `Filter`, `Keys`, `String`) and a structural `Validate`.
- **ent** (`protoc-gen-ent`) emits an optional `field.JSON("tags", map[string]string{})`, which ent
  stores in the dialect-appropriate JSON column.

Tags round-trip through Create, Get, List, and a full (no-field-mask) Update. Override the column
type with `column_type` if `jsonb` is not appropriate for your dialect.

### Filtering by tags

`List` accepts AIP-160 predicates over individual tag keys on both storage backends, evaluated as
dialect-aware JSON SQL. The tag key and value are always bind arguments, never interpolated:

| Filter | Meaning |
|---|---|
| `tags.env = "prod"` | the `env` tag equals `prod` |
| `tags.env != "prod"` | the `env` tag is present and not `prod` |
| `has(tags.team)` | the `team` tag key exists |

These predicates compose with `AND`, `OR`, `NOT`, and grouping like any other AIP-160 expression —
for example, `tags.env = "prod" AND has(tags.team)`. The generator emits a separate
`<Message>JSONColumns` map for tag paths, kept out of the scalar `<Message>Columns` map.

- **GORM** (`protoc-gen-storage`) renders the JSON SQL directly (Postgres `->>` / `jsonb_exists`,
  SQLite `json_extract`), with `List` passing the live dialect via `r.db.Dialector.Name()`.
- **ent** uses `entrepo.FilterPredicate`, which translates the parsed filter into ent predicates via
  `sqljson`. ent emits the dialect-correct JSON SQL when the query is built, so no dialect string
  is needed. The generated ent adapter (`<resource>_repo.ent.go`) wires this in `List` from the
  generated `<Message>EntColumns` / `<Message>EntJSONColumns` maps — no consumer code.

{{< callout type="info" >}}
**Not yet supported:** ordering by a tag key (`order_by=tags.env`); field-masked updates that name
`tags` (set tags with a full update instead); and, on the GORM backend, dialects other than Postgres
and SQLite (MySQL returns a clear error; ent's `sqljson` handles MySQL). A whole-map predicate
(`tags = "..."`) is intentionally unsupported — only per-key access (`tags.<key>`) is.
{{< /callout >}}

Tag policy — which keys and values are allowed and which combinations are permitted — is not part
of the type. `types.Tags` carries data only (its `Validate` checks structural well-formedness:
UTF-8 keys and values, length and count limits). Policy belongs to an external tag definition
service, reached through the `types.TagValidator` seam.

## Planned field kinds

`secret` is the first of a planned vocabulary of semantic field kinds, each expressed as an
`(infoblox.field.v1.opts)` annotation. The storage generators translate each kind into columns,
indexes, and (where relevant) filter operators, so a service declares intent in the proto and the
framework handles the implementation.

{{< callout type="warning" >}}
**The field kinds below are not yet available.** They are on the roadmap and are not in the
codegen today. They draw on prior art from
[atlas-app-toolkit](https://github.com/infobloxopen/atlas-app-toolkit). The *Today* column shows
the closest current alternative.
{{< /callout >}}

| Kind | What it will be | Today |
|---|---|---|
| **Typed ID** | a structured/opaque identifier kind (cf. atlas-app-toolkit's `atlas.rpc.Identifier`) rather than a bare string | a string `id` plus an AIP-122 resource name (see [Resource name (AIP-122)](#resource-name-aip-122)) |
| **Memo** | a first-class multi-line-text kind | a `string` field with `column_type: "text"` |
| **Case-insensitive text** | case-insensitive (CI) semantics wired through unique indexes, lookups, and filters | a `string` field with `column_type: "citext"` (Postgres) — the column compares case-insensitively, but unique/index/filter are not made CI automatically |

## Next

- [Define a Service](../define-a-service/) — the proto → generate → wire loop around this model.
- [Storage Shapes](../storage-shapes/) — persist the model with GORM or ent.
- [Secret Fields](../../secure/secret-fields/) — production secret handling with Vault Transit.
- [Annotations](../../../concepts/annotations/) — the complete `(infoblox.field.v1.opts)` reference.
- [Tenant Isolation](../../../concepts/tenant-isolation/) — how `account_id` scoping is enforced.
