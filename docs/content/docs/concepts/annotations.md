---
title: Annotations
weight: 2
---

devedge-sdk's contract is two proto annotations. They are **engine-neutral**: they name *what*
is required, not *how* it is evaluated, so they carry no policy-engine-specific fields.

The two annotations live in `infoblox/authz/v1/authz.proto` (the method rule) and
`infoblox/field/v1/field.proto` (the field options). The canonical schemas live in the
[`infobloxopen/apis`](https://github.com/infobloxopen/apis) module; the SDK depends on their
generated Go bindings (`github.com/infobloxopen/apis/proto/infoblox/authz/v1` and
`.../infoblox/field/v1`).

## `(infoblox.authz.v1.rule)` — method authorization

Attach a `Rule` to an RPC to declare its authorization requirement:

```proto
service ZoneService {
  rpc GetZone(GetZoneRequest) returns (Zone) {
    option (infoblox.authz.v1.rule) = {verb: "get", resource: "zone:{zone_id}"};
  }
  rpc CreateZone(CreateZoneRequest) returns (Zone) {
    option (infoblox.authz.v1.rule) = {verb: "create", resource: "zone"};
  }
  rpc HealthCheck(HealthRequest) returns (HealthResponse) {
    option (infoblox.authz.v1.rule) = {public: true}; // explicit, auditable opt-out
  }
}
```

The `Rule` message has three fields:

| Field | Number | Meaning |
|---|---|---|
| `verb` | 1 | the canonical permission verb. Standard set: `get`, `list`, `watch`, `create`, `update`, `delete`. Custom verbs (e.g. `download`) are allowed as free strings. `read` is intentionally *not* canonical — it maps to the **View** group (`get` + `list` + `watch`). |
| `resource` | 2 | the resource type or a template over request fields, e.g. `zone` or `zone:{zone_id}`. |
| `public` | 3 | if true, the method requires no authorization. **A method with neither a verb nor `public: true` is denied at runtime** and fails the boot-time completeness gate. |

Each annotation becomes one `authz.MethodRule` in Go:

```go
type MethodRule struct {
    Method   string // transport method id, e.g. "/dns.v1.ZoneService/GetZone"
    Verb     Verb   // the required verb; empty iff Public
    Resource string // resource type or template, e.g. "zone" or "zone:{zone_id}"
    Public   bool   // explicit no-authorization opt-out
}
```

### Two ways to consume it — pick per service

Both produce **identical** `[]authz.MethodRule`:

- **Reflection** — `authzpb.RulesFromGlobal()` reads the annotation off the linked descriptors
  at startup. No generated file.
- **Codegen** — `protoc-gen-devedge-authz` (run by `buf generate`) emits a
  `<Service>AuthzRules` table next to the `.pb.go`. Pass it to `server.Config.Rules` or
  `grpcauthz.WithRules(...)`.

That single set feeds **both** enforcement (the interceptor's rule table) **and** the permission
catalog (`catalog.Build`), which renders per-resource verbs, the endpoints implementing each, and
the View/Manage intent groups.

## `(infoblox.field.v1.opts).secret` — secret fields

Attach `FieldOptions` (the `infoblox.field.v1.opts` extension) to a message field to mark it
sensitive. The proto must `import "infoblox/field/v1/field.proto"`:

```proto
import "infoblox/field/v1/field.proto";

message APIKey {
  string id         = 1;
  string name       = 2;
  string account_id = 3;
  // key_value is raw API key material. Hashed for lookup, encrypted for recovery,
  // never stored as plaintext, never returned after creation.
  string key_value  = 4 [(infoblox.field.v1.opts) = {secret: true}];
  string key_prefix = 5; // first 8 chars, for display — NOT secret
}
```

`FieldOptions` also carries storage constraints and ORM relationship options; the field that drives
secret handling is `secret`:

| Field | Number | Meaning |
|---|---|---|
| `secret` | 1 | if true, the field contains sensitive data. The framework will: **encrypt/hash at rest** (never store plaintext), **redact in logs** (`[REDACTED]`), and **catch leaks** — the security-check tooling flags any code path that returns the raw value. |

A `secret` field drives behavior across three packages:

- **Storage** (`protoc-gen-storage` / `protoc-gen-ent`) emits `<field>_hash` and
  `<field>_cipher` columns instead of a plaintext column, and calls the `Encryptor` on
  create/update. See [Secret fields](../../guides/secret-fields/).
- **Logging** (`middleware/redact`) replaces the value with `[REDACTED]` before logging.
- **Security** (`seccheck.AssertNoSecretFieldsLeaked`) walks every response proto and fails if a
  secret field is non-empty (other than the literal `[REDACTED]`).

## `(infoblox.field.v1.opts)` — storage constraints

`secret` is the most visible option, but the same `FieldOptions` extension carries the storage
constraints and column overrides the codegen plugins translate into the generated model:

| Field | Number | Generates (GORM / ent) |
|---|---|---|
| `not_null` | 2 | `NOT NULL` constraint on the column (GORM tag `not null`). |
| `unique` | 3 | `UNIQUE` index. **Tenant-aware:** in a message with an `account_id` field it generates a *composite* unique index over `(account_id, <field>)`; otherwise a global one. |
| `index` | 4 | non-unique index (GORM tag `index`). |
| `column_name` | 5 | overrides the default snake_case column name (GORM tag `column:<name>`). |
| `column_type` | 6 | overrides the DB column type, e.g. `varchar(255)` (GORM tag `type:<type>`). |

```proto
message Widget {
  string id    = 1;
  string slug  = 2 [(infoblox.field.v1.opts) = {unique: true, not_null: true}];
  string notes = 3 [(infoblox.field.v1.opts) = {column_type: "text"}];
}
```

`unique` takes precedence over `index` (a unique index already covers the lookup). The constraints
are applied by `db.AutoMigrate`; for production schemas drive DDL through
[`infobloxopen/migrate`](../../reference/persistence/#migrations) rather than relying on
AutoMigrate.

{{< callout type="info" >}}
**`unique` is per-tenant by default.** When the message has an `account_id` field (the multi-tenant
case), `unique` does **not** generate a global index on the column alone — that would let one tenant
deny another the use of a common name (e.g. `primary`) and leak that the name exists. Instead the
field joins `account_id` in a composite unique index named `ux_<message>_account_<field>`, with
`account_id` as the leading column. So the example `Widget.slug` above is unique *within each
account*. A message with no `account_id` field keeps a plain global unique index.
{{< /callout >}}

## `(infoblox.field.v1.opts)` — relationships

`FieldOptions` also declares ORM relationships on a message-typed (or repeated message-typed)
field. `protoc-gen-storage` emits the corresponding GORM association tag (and `protoc-gen-ent` the
equivalent edge):

The generated association is typed against the related message's **GORM model** —
`*<Related>Model` (or `[]*<Related>Model`) — so `Preload`, FK constraints, and joins all work; the
`foreign_key` you give in snake_case is emitted as the related model's Go field name.

| Option | Number | Declare on | Generates (GORM) |
|---|---|---|---|
| `has_one` | 20 | a message field (1:1, FK on the *other* table) | `*<Related>Model gorm:"foreignKey:<Fk>"` |
| `has_many` | 21 | a repeated message field (1:N, FK on the *other* table) | `[]*<Related>Model gorm:"foreignKey:<Fk>"` |
| `belongs_to` | 22 | a message field (the inverse — FK on *this* table) | `*<Related>Model gorm:"foreignKey:<Fk>"` + the FK column field |
| `many_to_many` | 23 | a repeated message field (N:N via a join table) | `[]*<Related>Model gorm:"many2many:<join_table>"` |

```proto
import "infoblox/field/v1/field.proto";

message Order {
  string id      = 1;
  string user_id = 2;
  // belongs_to: the FK (user_id) lives on the orders table.
  User   user    = 3 [(infoblox.field.v1.opts) = {belongs_to: {foreign_key: "user_id"}}];
  // has_many: line_items.order_id is the FK on the other table.
  repeated LineItem line_items = 4 [(infoblox.field.v1.opts) = {has_many: {foreign_key: "order_id"}}];
}
```

For `belongs_to`, the natural AIP shape is to expose the FK as a scalar field **and** annotate the
association — exactly the `Order` above, which has both `user_id` and `belongs_to user`. The
generated model carries the scalar FK column once (reused by the association) and a
`*UserModel` association; it does not duplicate the FK. If you annotate `belongs_to` *without* a
sibling scalar FK field, the generator emits the FK column for you.

`has_one`/`has_many`/`belongs_to` accept `foreign_key` and `association_foreign_key`; `has_many`
also accepts `position_field` for an ordered association; `many_to_many` takes `join_table`,
`foreign_key`, and `association_foreign_key`. A scalar repeated field with no relationship option is
skipped in the GORM model (it needs JSONB serialization), so reach for these options whenever a
field references another resource. The related message must itself be a stored resource (it has an
`id` field, so `<Related>Model` is generated).

## Extension numbers

The annotations are protobuf custom options:

```proto
extend google.protobuf.MethodOptions {
  Rule rule = 50001;          // (infoblox.authz.v1.rule)
}
extend google.protobuf.FieldOptions {
  FieldOptions opts = 50003;  // (infoblox.field.v1.opts)
}
```

Both numbers (`50001`, `50003`) are in the protobuf **50000–99999 "internal use"** range. Before
any cross-org publication, obtain a globally-unique number from the protobuf registry.

{{< callout type="warning" >}}
The copy of `authz.proto` checked into the SDK repo is a **mirror** for codegen input only — its
`go_package` points at the canonical `infobloxopen/apis` module, so no Go is generated from the
local copy. Keep it byte-identical to the canonical file.
{{< /callout >}}
