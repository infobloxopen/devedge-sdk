---
title: Annotations
weight: 2
---

Annotations are protobuf custom options that declare authorization requirements, field
sensitivity, storage constraints, and aggregate boundaries directly in your `.proto` files.
They let code generators and runtime enforcement share a single authoritative source of truth
rather than requiring you to duplicate declarations across generated code and configuration.

The annotations are engine-neutral: they name *what* is required, not *how* it is evaluated, so
they carry no policy-engine-specific fields. This is what distinguishes them from OPA-embedded
or otherwise engine-coupled annotations.

Use annotations when you define a new RPC, add a sensitive field, configure storage columns, or
establish aggregate ownership rules.

The two core annotation packages are `infoblox/authz/v1/authz.proto` (method rules) and
`infoblox/field/v1/field.proto` (field options). Their canonical schemas live in the
[`infobloxopen/apis`](https://github.com/infobloxopen/apis) module; the SDK depends on their
generated Go bindings (`github.com/infobloxopen/apis/proto/infoblox/authz/v1` and
`.../infoblox/field/v1`). A third package, `infoblox/ddd/v1/ddd.proto`, is defined and owned by
this SDK and generated locally in-repo.

## Method authorization

Attach a `(infoblox.authz.v1.rule)` option to an RPC to declare its authorization requirement:

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
| `verb` | 1 | The canonical permission verb. The standard set is: `get`, `list`, `watch`, `create`, `update`, `delete`. Custom verbs such as `download` are allowed as free strings. The verb `read` is not in the canonical set — it maps to the View group, which covers `get`, `list`, and `watch`. |
| `resource` | 2 | The resource type or a template over request fields, for example `zone` or `zone:{zone_id}`. |
| `public` | 3 | If `true`, the method requires no authorization. |

{{< callout type="warning" >}}
**A method with neither a `verb` nor `public: true` is denied at runtime.** It also fails the boot-time completeness gate. Every RPC must have an explicit rule.
{{< /callout >}}

Each annotation becomes one `authz.MethodRule` in Go:

```go
type MethodRule struct {
    Method   string // transport method id, e.g. "/dns.v1.ZoneService/GetZone"
    Verb     Verb   // the required verb; empty iff Public
    Resource string // resource type or template, e.g. "zone" or "zone:{zone_id}"
    Public   bool   // explicit no-authorization opt-out
}
```

### Consuming the rules

Both approaches produce an identical `[]authz.MethodRule`:

- **Reflection** — `authzpb.RulesFromGlobal()` reads the annotation off the linked descriptors
  at startup. No generated file is required.
- **Codegen** — `protoc-gen-devedge-authz` (run by `buf generate`) emits a
  `<Service>AuthzRules` table next to the `.pb.go`. Pass it to `server.Config.Rules` or
  `grpcauthz.WithRules(...)`.

That single rule set feeds both enforcement (the interceptor's rule table) and the permission
catalog (`catalog.Build`), which renders per-resource verbs, the endpoints implementing each,
and the View/Manage intent groups.

## Secret fields

Attach `(infoblox.field.v1.opts)` with `secret: true` to a message field to mark it
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

| Field | Number | Meaning |
|---|---|---|
| `secret` | 1 | If `true`, the field contains sensitive data. The framework will **encrypt/hash at rest** (never store plaintext), **redact in logs** (`[REDACTED]`), and **catch leaks** — the security-check tooling flags any code path that returns the raw value. |

A `secret: true` field drives behavior across three packages:

- **Storage** (`protoc-gen-storage` / `protoc-gen-ent`) emits `<field>_hash` and
  `<field>_cipher` columns instead of a plaintext column, and calls the `Encryptor` on
  create/update. See [Secret fields](../../how-to/model-and-persist/model-a-resource/#secret-fields).
- **Logging** (`middleware/redact`) replaces the value with `[REDACTED]` before logging.
- **Security** (`seccheck.AssertNoSecretFieldsLeaked`) walks every response proto and fails if a
  secret field is non-empty (other than the literal `[REDACTED]`).

## Storage constraints

The `(infoblox.field.v1.opts)` extension also carries storage constraints and column overrides
that the codegen plugins translate into the generated model:

| Field | Number | Generates (GORM / ent) |
|---|---|---|
| `not_null` | 2 | `NOT NULL` constraint on the column (GORM tag `not null`). |
| `unique` | 3 | `UNIQUE` index. **Tenant-aware:** in a message with an `account_id` field it generates a *composite* unique index over `(account_id, <field>)`; otherwise a global one. |
| `index` | 4 | non-unique index (GORM tag `index`). |
| `column_name` | 5 | overrides the default snake_case column name (GORM tag `column:<name>`). |
| `column_type` | 6 | overrides the DB column type, e.g. `varchar(255)` (GORM tag `type:<type>`). |
| `unique_with` | 8 | sibling field names that join a `unique` field's per-tenant composite, forming a "unique within a parent" index over `(account_id, <unique_with...>, <field>)`. Requires `unique` and an `account_id` field. |
| `allowed_values` | 9 | restricts a string field to a fixed set of values; the generated Create and Update handlers reject an out-of-set value with `InvalidArgument`. |

```proto
message Widget {
  string id    = 1;
  string slug  = 2 [(infoblox.field.v1.opts) = {unique: true, not_null: true}];
  string notes = 3 [(infoblox.field.v1.opts) = {column_type: "text"}];
}
```

`unique` takes precedence over `index` because a unique index already covers the lookup. The
constraints are applied by `db.AutoMigrate`; for production schemas, drive DDL through
[`infobloxopen/migrate`](../../reference/persistence/#migrations) rather than relying on
AutoMigrate.

{{< callout type="info" >}}
**`unique` is per-tenant by default.** When the message has an `account_id` field (the
multi-tenant case), `unique` does **not** generate a global index on the column alone — that
would let one tenant deny another the use of a common name (such as `primary`) and leak that
the name exists. Instead, the field joins `account_id` in a composite unique index named
`ux_<message>_account_<field>`, with `account_id` as the leading column. So the example
`Widget.slug` above is unique *within each account*. A message with no `account_id` field
keeps a plain global unique index.
{{< /callout >}}

## ORM relationships

`(infoblox.field.v1.opts)` also declares ORM relationships on a message-typed (or repeated
message-typed) field. `protoc-gen-storage` emits the corresponding GORM association tag, and
`protoc-gen-ent` emits the equivalent edge.

The generated association is typed against the related message's GORM model —
`*<Related>Model` (or `[]*<Related>Model`) — so `Preload`, FK constraints, and joins all work.
The `foreign_key` you provide in snake_case is emitted as the related model's Go field name.

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

For `belongs_to`, the natural AIP shape is to expose the FK as a scalar field **and** annotate
the association — exactly the `Order` above, which has both `user_id` and `belongs_to user`. The
generated model carries the scalar FK column once (reused by the association) and a
`*UserModel` association; it does not duplicate the FK. If you annotate `belongs_to` *without* a
sibling scalar FK field, the generator emits the FK column for you.

**On the ent backend**, the same annotations become ent edges. A `has_many` on the parent and a
`belongs_to` on the child are emitted as one inverse pair — `edge.To("line_items", LineItem.Type)`
on `Order` and `edge.From("user", User.Type).Ref(...).Unique().Field("user_id")` on the child — and
the scalar FK is bound to the edge with `.Field(...)` so ent generates a single `SetUserID` setter
(the scalar column and the association share one column, mirroring the GORM behavior). See
[Codegen → Relationships in ent](../../reference/codegen/#relationships-in-ent) for the generated
shape and `testdata/fleet/` for a complete worked example.

`has_one`, `has_many`, and `belongs_to` accept `foreign_key` and `association_foreign_key`.
`has_many` also accepts `position_field` for an ordered association. `many_to_many` takes
`join_table`, `foreign_key`, and `association_foreign_key`.

{{< callout type="info" >}}
**A scalar repeated field with no relationship option is skipped in the GORM model** and
requires JSONB serialization. Use one of the relationship options whenever a field references
another resource. The related message must itself be a stored resource — it must have an `id`
field so that `<Related>Model` is generated.
{{< /callout >}}

## Full-text search

Attach `searchable: true` to a plain field, and/or a message-level `(infoblox.storage.v1.search)`
option, to declare a resource's full-text search surface — the fields and calculated values that a
List call's `q` operator matches against. See [Add full-text search to a resource](../../how-to/model-and-persist/add-full-text-search/)
for the task recipe and [Persistence reference → Full-text search (`q`)](../../reference/persistence/#full-text-search-q)
for the generated predicate.

```proto
import "infoblox/field/v1/field.proto";
import "infoblox/storage/v1/storage.proto";

message Widget {
  option (infoblox.storage.v1.search) = {
    strategy: STRATEGY_JIT
    sources: [
      { name: "category_label", exprs: { expr: [
        { flavor: "sql", dialect: "postgres", version: "1",
          expr: "CASE category WHEN 'premium' THEN 'tier premium deluxe' ELSE 'tier none' END" },
        { flavor: "cel", version: "1",
          expr: "msg.category == 'premium' ? 'tier premium' : 'tier none'" }
      ]}}
    ]
  };
  string display_name = 3 [(infoblox.field.v1.opts) = {searchable: true}];
  string category      = 4 [(infoblox.field.v1.opts) = {allowed_values: ["standard", "premium"]}];
}
```

The `cel` expression alongside the `sql` one keeps `category_label` portable to the SQLite
dev/test driver — see the warning below.

| Field (`FieldOptions`) | Number | Meaning |
|---|---|---|
| `searchable` | 12 | Includes this field's value in the resource's full-text search vector. The field must be `string`, `enum`, a string-typed repeated field, `map<string, string>` tags, or `Timestamp`, and must not be `secret` or `INPUT_ONLY`. |

`(infoblox.storage.v1.search)` is a **message** option that declares the rest of the searchable
surface: the materialization strategy, the Postgres text-search configuration, and any calculated
sources beyond field-flagged columns.

`SearchConfig`:

| Field | Number | Meaning |
|---|---|---|
| `strategy` | 1 | Materialization strategy (`Strategy`, below). Default `STRATEGY_JIT`. |
| `text_config` | 2 | Postgres text-search configuration, e.g. `simple` or `english`. Default `"simple"`. |
| `sources` | 3 | Calculated/transformed sources beyond field-flagged columns (repeated `SearchSource`). |

`SearchConfig.Strategy`:

| Value | Meaning |
|---|---|
| `STRATEGY_UNSPECIFIED` | Treated as `STRATEGY_JIT`. |
| `STRATEGY_JIT` | The search vector is computed at query time. No migration. The default. |
| `STRATEGY_INDEXED` | The search vector is persisted as a generated column and a GIN index. |
| `STRATEGY_PROJECTED` | **Reserved.** Declaring it is valid schema, but `make generate` fails loud — this strategy has no local implementation. It marks a resource whose search surface is materialized remotely, for a future cross-service search index. |

`SearchSource`:

| Field | Number | Meaning |
|---|---|---|
| `name` | 1 | Logical name for diagnostics and the `x-aip-search` OpenAPI extension. |
| `field` | 2 | (oneof `from`) References an existing message field by name — a portable source. On PostgreSQL, the field's `@` and `.` characters are normalized to spaces before indexing (`alice@acme.com` tokenizes as `alice acme com`); the SQLite fallback does not apply this normalization. |
| `exprs` | 3 | (oneof `from`) A `SearchExprSet` of flavored, calculated expressions. |
| `text_config` | 4 | Overrides the message-level `text_config` for this source only. |

`SearchExprSet` / `SearchExpr`:

| Field | Number | Meaning |
|---|---|---|
| `SearchExprSet.expr` | 1 | Repeated `SearchExpr` — the flavored expressions contributing to one calculated source. |
| `SearchExpr.flavor` | 1 | Expression language: `sql` or `cel`. |
| `SearchExpr.dialect` | 2 | Target backend for a `sql` expression. Only `postgres` is supported. |
| `SearchExpr.version` | 3 | Flavor spec version, pinning the expression's meaning. |
| `SearchExpr.expr` | 4 | The expression text. |

{{< callout type="warning" >}}
**A `sql` expression is Postgres-only unless paired with a `cel` expression in the same source.**
`cel` compiles to both a Postgres and a SQLite expression, so it is the portable choice for a
calculated value. A `sql`-only source generates without error on every backend, but fails at
**runtime**, not at generation time: querying it over a non-Postgres connection returns
`Unimplemented` instead of running broken SQL. See [Add full-text search to a resource → SQLite and `sql`-flavor sources](../../how-to/model-and-persist/add-full-text-search/#sqlite-and-sql-flavor-sources).
{{< /callout >}}

## Aggregate boundaries

The `infoblox.ddd.v1` annotations declare domain-driven design (DDD) aggregate boundaries.
Unlike `authz.v1` and `field.v1`, which are consumed as bindings from the published
`infobloxopen/apis` module, `ddd.v1` is defined and owned by this SDK. Its Go binding is
generated locally, in-repo — safe precisely because the namespace is SDK-private, so there is
no register-once collision with another module.

| Option | Number | Declare on | Meaning |
|---|---|---|---|
| `aggregate {root: true}` | 50010 | a message | the message is an aggregate **root** (consistency boundary) |
| `member {root: "<Root>"}` | 50011 | a message | the message is a **member** owned by the named root (containment) |
| `references {aggregate, foreign_key}` | 50012 | a message field | a **cross-aggregate** link: scalar FK + ID, **no** traversable edge |

```proto
import "infoblox/ddd/v1/ddd.proto";

message Order { option (infoblox.ddd.v1.aggregate) = {root: true}; /* ... */ }
message Item  { option (infoblox.ddd.v1.member) = {root: "Order"}; /* ... */ }

message ApiKey {
  // a scalar user_id FK + ID, no edge into the User aggregate.
  User user = 6 [(infoblox.ddd.v1.references) = {aggregate: "User", foreign_key: "user_id"}];
}
```

`member` and `aggregate` drive containment cascade, member write-redirection, and the
fail-closed boundary gate. `references` keeps cross-aggregate links ID-only. See
[Aggregates](../aggregates/) for the full model and `testdata/iam/` for a complete worked
example.

## Extension numbers

The annotations are protobuf custom options:

```proto
extend google.protobuf.MethodOptions {
  Rule rule = 50001;          // (infoblox.authz.v1.rule)
}
extend google.protobuf.FieldOptions {
  FieldOptions opts = 50003;  // (infoblox.field.v1.opts)
  References references = 50012; // (infoblox.ddd.v1.references)
}
extend google.protobuf.MessageOptions {
  Aggregate aggregate = 50010;    // (infoblox.ddd.v1.aggregate)
  Member member = 50011;          // (infoblox.ddd.v1.member)
  SearchConfig search = 50051;    // (infoblox.storage.v1.search)
}
```

The numbers `50001`, `50003`, `50010`–`50012`, and `50051` fall in the protobuf **50000–99999
"internal use"** range.

{{< callout type="warning" >}}
**Obtain a globally-unique extension number before any cross-org publication.** Register the
number with the protobuf registry to avoid collisions with other organizations.
{{< /callout >}}

{{< callout type="warning" >}}
**The copies of `authz.proto` and `field.proto` checked into the SDK repo are mirrors for
codegen input only.** Their `go_package` points at the canonical `infobloxopen/apis` module,
so no Go is generated from the local copy. Keep them byte-identical to the canonical file.
The SDK-owned `ddd.proto` is the exception: it is generated locally because the
`infoblox.ddd.v1` namespace has no canonical counterpart.
{{< /callout >}}
