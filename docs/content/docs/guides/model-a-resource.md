---
title: Model a Resource
weight: 2
aliases:
  - /docs/guides/secret-fields/
---

In devedge-sdk the **proto message is your data model**. [Define a Service](../define-a-service/)
covers the generate-and-wire loop; this guide covers what goes *inside* the message: what turns it
into a stored resource, which field types persist, the framework-managed fields, the storage
constraints, and how resources relate to one another.

## What makes a message a resource

The storage generators (`protoc-gen-storage`, `protoc-gen-ent`) turn a message into a persisted
resource — a `<Name>Model` / ent schema plus a `<Name>Repository` — when **both** are true:

1. it has an **`id` field** (a string; it becomes the primary key, `varchar(36)`), and
2. it is **not** an RPC wrapper — names ending in `Request` or `Response` are skipped.

Everything else (request/response messages, value objects like an `OperationStatus` with no `id`)
is left alone. So the smallest possible resource is:

```proto
message Widget {
  string id           = 1;
  string display_name = 2;
}
```

That alone generates a `WidgetModel`, a CRUDL `WidgetRepository`, and the columns `id`,
`display_name`, plus the framework's automatic `created_at`, `updated_at`, and `etag` columns.

## Give it a resource name (AIP-122)

Optionally attach `(google.api.resource)` to mark the resource type and its name pattern. The
generator then emits a `name` value, a `<Name>NamePattern` constant, and `Format<Name>Name` /
`Parse<Name>Name` helpers (backed by [`persistence/resourcename`](../../reference/persistence/)):

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

`name` is `OUTPUT_ONLY` (see below) — it is derived from `id`, never stored or accepted on input.

## Field types

Scalar proto types map straight to columns:

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
**Not every type is a column.** Repeated scalars and nested messages **without** a
[relationship annotation](#relationships) are skipped (they need a JSON/JSONB serialization
strategy that isn't generated yet) — the one exception is a `map<string, string>`, which persists
as a JSONB column (see [Tags](#tags)). **Enums** are not mapped to a typed column either — model an
enum as a `string` (or an integer field) if you need it persisted, and validate it at the API
boundary. `google.protobuf.Timestamp` only carries built-in storage meaning for the two
[framework fields](#framework-managed-fields) below; other timestamp fields aren't persisted by the
generator today.
{{< /callout >}}

## Framework-managed fields

A handful of well-known field names get behavior for free. Declare them and the framework wires the
storage and lifecycle; you never write the plumbing.

| Field (proto) | Type | What it gives you |
|---|---|---|
| `account_id` | `string` | **Tenant scope.** Every query is automatically filtered by `account_id` from `middleware.TenantIDFromContext(ctx)` — see [Tenant Isolation](../../concepts/tenant-isolation/). It also makes `unique` per-tenant (below). |
| `etag` | `string` `OUTPUT_ONLY` | **AIP-154 concurrency.** Stamped on every write, surfaced on read; a client echoes it as `If-Match` for a 412-guarded conditional update. Auto-stamped and surfaced on **both** backends with no consumer code — ent via the generated `EtagMixin` plus the generated `fromEnt` projection — see [codegen → ETag](../../reference/codegen/). |
| `delete_time` | `Timestamp` `OUTPUT_ONLY` | **AIP-148 soft-delete.** Opts the resource into soft-delete (a `DeletedAt` column); `Delete` sets it, `Undelete` clears it, `List` hides soft-deleted rows unless `show_deleted`. Omit the field for hard-delete. A per-tenant `unique` key on a soft-delete resource is **re-creatable** once the holder is soft-deleted — see [codegen → Soft-delete + unique](../../reference/codegen/) (`dialect=mysql` for the MySQL strategy). |
| `expire_time` | `Timestamp` `OUTPUT_ONLY` | **AIP-148 TTL.** Adds an `expire_time` column and a `PurgeExpired` method to the repository. |
| `created_at`, `updated_at` | — | Added to every model automatically; you don't declare them. |

`OUTPUT_ONLY` (from `google.api.field_behavior`) marks a field as **server-managed** — it is
returned to clients but never read from a create/update request, so it is never persisted from
input. This is the apikey resource, which uses four of these at once:

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

`(infoblox.field.v1.opts)` carries the per-field storage constraints the generators translate into
the model:

| Option | Effect |
|---|---|
| `not_null` | `NOT NULL` on the column |
| `unique` | unique index — **per-tenant** (composite with `account_id`) when the message has an `account_id` field, otherwise global |
| `index` | non-unique index |
| `column_name` | override the default snake_case column name |
| `column_type` | override the DB column type, e.g. `varchar(255)` |

```proto
message Widget {
  string id    = 1;
  string slug  = 2 [(infoblox.field.v1.opts) = {unique: true, not_null: true}];
  string notes = 3 [(infoblox.field.v1.opts) = {column_type: "text"}];
}
```

{{< callout type="info" >}}
**`unique` is per-tenant by default.** In a message with `account_id`, a `unique` field joins
`account_id` in a composite index (account_id leading) so one tenant's names don't collide with —
or leak to — another's. The [Annotations concept](../../concepts/annotations/) has the full option
reference and the rationale.
{{< /callout >}}

## Relationships

Model how resources connect by annotating a **message-typed** (or **repeated message-typed**) field.
The generators emit the matching GORM association and ent edge:

| Option | Declare on | Cardinality / FK |
|---|---|---|
| `has_one` | a message field | 1:1, FK on the *other* table |
| `has_many` | a repeated message field | 1:N, FK on the *other* table |
| `belongs_to` | a message field | the inverse — FK on *this* table |
| `many_to_many` | a repeated message field | N:N via a join table |

The **natural AIP shape** for a parent/child pair is to expose the foreign key as a scalar field
(so clients can read and filter it) *and* annotate the association. This is the `fleet` fixture — a
`Fleet` has many `Vehicle`s; each `Vehicle` belongs to one `Fleet` via a scalar `fleet_id`:

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
typed associations (`[]*VehicleModel`, `*FleetModel`) you can `Preload`; on the **ent** backend they
become graph edges — ent's strength for relationship-heavy domains. See
[Storage Shapes](../storage-shapes/) for when to choose ent, and the
[Codegen reference](../../reference/codegen/) for the exact generated shape.

## Secret fields

`secret` is the field kind whose handling spans storage, logging, and the security gate. A field
marked `secret` is **never stored as plaintext and never returned after creation**:

```proto
string key_value = 4 [(infoblox.field.v1.opts) = {secret: true}];
```

### Stored as hash + cipher, never plaintext

`protoc-gen-storage` does not emit a plaintext column. It emits two — a deterministic **hash** (for
lookup) and a **cipher** (for recovery) — and omits the plaintext from the model entirely:

```go
type APIKeyModel struct {
    ID             string `gorm:"primaryKey;type:varchar(36)"`
    // ...
    KeyValueHash   string `gorm:"column:key_value_hash;index"` // for lookup
    KeyValueCipher string `gorm:"column:key_value_cipher"`      // for recovery
    // ...
}
```

The `toModel`/`fromModel` helpers skip secret fields, so plaintext can never round-trip through the
model; encryption happens explicitly in `Create`/`Update`. Because the repository needs an
`Encryptor`, its generated constructor takes one:

```go
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
```

Since the plaintext is gone, look a record up by its raw value through the generated
`LookupBy<Field>Hash` method (tenant-scoped):

```go
h, _ := enc.Hash(ctx, presentedKey)
key, err := repo.LookupByKeyValueHash(ctx, h)
```

### Choose an encryptor

Both implementations satisfy one interface (`Encrypt` / `Decrypt` / `Hash`):

- **Dev** — `secret.NewDev(key)` uses AES-256-GCM + HMAC-SHA256, all in-process (the key must be
  ≥ 32 bytes). Ideal for local dev and tests; **never use it in production.**
- **Production** — `secret.NewVaultTransit(addr, token, keyName)` calls HashiCorp Vault's Transit
  engine over plain HTTP (no Vault SDK dependency). See [Vault Transit](../vault-transit/).

The full `Encryptor` API is in the [secret reference](../../reference/secret/).

### Also redacted and leak-checked

The `secret` annotation does more than storage:

- **Logs** — `middleware/redact` replaces the value with `[REDACTED]` before logging.
- **Responses** — `seccheck.AssertNoSecretFieldsLeaked(resp...)` walks every response proto and
  fails if a secret field holds anything but `[REDACTED]`. Wire it into your tests — see
  [Security Check](../security-check/).

{{< callout type="warning" >}}
Your handler still receives the raw value on the **request**. Return it to the caller **once**, at
creation time if at all — never on `Get`/`List`. `AssertNoSecretFieldsLeaked` is your safety net.
{{< /callout >}}

## Tags

A `map<string, string>` field is the **Tags** field kind: a free-form set of string labels
persisted as a single JSONB column. No annotation is needed — the storage generators detect a
`map<string, string>` and wire it automatically.

```proto
message APIKey {
  string id = 2;
  // ...
  map<string, string> tags = 10;
}
```

- **GORM** (`protoc-gen-storage`) emits a `types.Tags` column tagged `gorm:"type:jsonb"`.
  `github.com/infobloxopen/devedge-sdk/types.Tags` is an ORM-agnostic `map[string]string` that
  implements `driver.Valuer` / `sql.Scanner` (the atlas custom-field-type convention), so it
  persists transparently. `jsonb` is native on Postgres and works on SQLite via type affinity. The
  type ships helpers (`Merge`, `Clone`, `Filter`, `Keys`, `String`) and a structural `Validate`.
- **ent** (`protoc-gen-ent`) emits an optional `field.JSON("tags", map[string]string{})`, which ent
  stores in the dialect-appropriate JSON column.

Tags round-trip through Create, Get, List, and a full (no-field-mask) Update. Override the column
type with `column_type` if `jsonb` isn't right for your dialect.

### Filtering by tags

`List` accepts AIP-160 predicates over individual tag keys on **both** storage backends, evaluated
as dialect-aware JSON SQL. The tag key and value are always bind arguments, never interpolated:

| Filter | Meaning |
|---|---|
| `tags.env = "prod"` | the `env` tag equals `prod` |
| `tags.env != "prod"` | the `env` tag is present and not `prod` |
| `has(tags.team)` | the `team` tag key exists |

These compose with `AND` / `OR` / `NOT` and grouping like any other predicate, e.g.
`tags.env = "prod" AND has(tags.team)`. The generator emits a separate `<Message>JSONColumns` map
for tag paths, kept out of the scalar `<Message>Columns` map.

- **GORM** (`protoc-gen-storage`) renders the JSON SQL directly (Postgres `->>` / `jsonb_exists`,
  SQLite `json_extract`), with `List` passing the live dialect via `r.db.Dialector.Name()`.
- **ent** uses `entrepo.FilterPredicate`, which translates the parsed filter into ent predicates via
  `sqljson` — ent emits the dialect-correct JSON SQL when the query is built, so no dialect string
  is needed. The **generated** ent adapter (`<resource>_repo.ent.go`) already wires this in `List`
  from the generated `<Message>EntColumns` / `<Message>EntJSONColumns` maps — no consumer code.

{{< callout type="info" >}}
**Still out of scope**, tracked as follow-ups: ordering by a tag key (`order_by=tags.env`);
field-masked updates that name `tags` (set tags with a full update); and, on the GORM backend,
dialects other than Postgres/SQLite (MySQL returns a clear error — ent's `sqljson` handles MySQL).
A bare `tags = "..."` (whole-map) predicate is intentionally unsupported — only `tags.<key>` access is.
{{< /callout >}}

**Semantic tag policy** — which keys/values are allowed and which combinations are permitted — is
*not* part of the type. `types.Tags` carries data only (its `Validate` checks structural
well-formedness: UTF-8 keys/values, length and count limits). Policy belongs to an external tag
definition service, reached through the `types.TagValidator` seam.

## Planned field kinds

`secret` is the first of what will be a vocabulary of **semantic field kinds** — each a
`(infoblox.field.v1.opts)` annotation the storage generators translate into columns, indexes, and
(where relevant) filter operators, so a service declares intent in the proto and the framework does
the rest.

{{< callout type="warning" >}}
**Not yet available.** The kinds below are on the roadmap — they are **not** in the codegen today.
They capture direction and the prior art devedge-sdk draws from
([atlas-app-toolkit](https://github.com/infobloxopen/atlas-app-toolkit)); the *Today* column is the
closest thing you can express right now.
{{< /callout >}}

| Kind | What it will be | Today |
|---|---|---|
| **Typed ID** | a structured/opaque identifier kind (cf. atlas-app-toolkit's `atlas.rpc.Identifier`) rather than a bare string | a string `id` plus an AIP-122 resource name (see *Give it a resource name* above) |
| **Memo** | a first-class multi-line-text kind | a `string` field with `column_type: "text"` |
| **Case-insensitive text** | CI semantics wired through unique indexes, lookups, and filters | a `string` field with `column_type: "citext"` (Postgres) — the column compares case-insensitively, but unique/index/filter aren't made CI automatically |

Want one of these sooner, or have another field kind in mind? It's a small spec away — the pattern
(`secret`) already exists to copy.

## Next

- [Define a Service](../define-a-service/) — the proto → generate → wire loop around this model.
- [Storage Shapes](../storage-shapes/) — persist the model with GORM or ent.
- [Vault Transit](../vault-transit/) — production secret handling with Vault.
- [Annotations](../../concepts/annotations/) — the complete `(infoblox.field.v1.opts)` reference.
- [Tenant Isolation](../../concepts/tenant-isolation/) — how `account_id` scoping is enforced.
