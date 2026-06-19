---
title: codegen
weight: 6
---

The SDK ships protoc plugins that turn your proto into running code. They live as `main` packages
under `cmd/` and are invoked by `buf generate`.

```bash
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-svc@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-storage@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-ent@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-devedge-authz@latest
```

| Plugin | Reads | Emits |
|---|---|---|
| `protoc-gen-devedge-authz` | `(infoblox.authz.v1.rule)` | `<Service>AuthzRules []authz.MethodRule` |
| `protoc-gen-svc` | the service definition | the service scaffold (`*.svc.go`) |
| `protoc-gen-storage` | messages (+ `field.secret`, `account_id`) | a GORM `Repository` (`*.storage.go`) |
| `protoc-gen-ent` | resource messages (those with an `id`) | an ent schema (`ent/schema/*.go`) |

## protoc-gen-devedge-authz

Emits a generated `[]authz.MethodRule` table from the method annotations — the same rules
`authzpb.RulesFromGlobal()` would produce by reflection, but as a checked-in file. Pass it to
`server.Config.Rules` (or `grpcauthz.WithRules`).

## protoc-gen-svc

Generates a single registration helper, `Register<Service>(s *server.Server, impl <Service>Server) error`.
It runs the **boot-time authz completeness gate** (`grpcauthz.AssertMethodsDeclared` over
`s.Rules()`), registers your implementation on the gRPC server, and registers the HTTP/JSON gateway
handler — all in one call. It does **not** generate handler bodies or repository wiring: you write
the `<Service>Server` methods over a `persistence.Repository`. (The `*.storage.go` from
`protoc-gen-storage` gives you that repository.)

## protoc-gen-storage

Generates a GORM-backed `Repository` for each message. For a message named `APIKey` it emits:

- **`APIKeyModel`** — the GORM model. Standard columns plus `ETag`, `CreatedAt`, `UpdatedAt` — and,
  **only for resources that opt into soft-delete** (see below), a `DeletedAt` column of type
  `gorm.DeletedAt`. (The `APIKey` example opts in, so its model has `DeletedAt`.) The `ETag` column
  is populated only when the resource declares an `etag` field (see *ETag* below).
- **`toModel_APIKey` / `fromModel_APIKey`** — converters. They **skip** repeated fields (TODO:
  JSONB), nested messages (TODO: serialization), and secret fields.
- **`APIKeyRepository`** + **`NewAPIKeyRepository`** — `Get`, `List`, `Create`, `Update`,
  `Delete`, satisfying `persistence.Repository[*APIKey, string]` (a compile-time `var _` check is
  emitted).

### Soft-delete (opt-in, AIP-148/149)

Soft-delete is **opt-in per resource**. A message becomes soft-deletable when it declares a
server-managed `delete_time` timestamp:

```protobuf
google.protobuf.Timestamp delete_time = 7 [(google.api.field_behavior) = OUTPUT_ONLY];
```

The field name must be `delete_time`, the type `google.protobuf.Timestamp`, and it must be
`OUTPUT_ONLY` (it is set by the framework, never written by the client). When present,
`protoc-gen-storage` emits the full soft-delete shape:

- a `DeletedAt` column (`gorm.DeletedAt`, indexed) on the model;
- `Delete` performs a **soft** delete (stamps `deleted_at`) instead of a physical row delete;
- `List` excludes soft-deleted rows unless `ListOptions.ShowDeleted` is set;
- `Undelete` (AIP-149) clears `deleted_at` and restores the row.

**Without a `delete_time` field a resource is hard-delete by default:** `Delete` physically removes
the row, `List` has no deleted-row filter, and `Undelete` is a stub that always returns
`persistence.ErrNotFound` (so the `Repository` interface stays uniform). Adding the `delete_time`
field is the only switch — there is no separate annotation or option.

#### Soft-delete + `unique`: re-creating a key, and the `dialect` option

When a resource is **both** soft-delete **and** has a per-tenant `unique` field (e.g. `source_ref`),
the per-tenant composite unique `(account_id, <field>)` must not let a *soft-deleted* row keep the key
reserved — you must be able to create a fresh resource with the same value after the old one is
soft-deleted (and `Undelete` correctly fails with a conflict if the key was taken meanwhile). The
codegen handles this for you, with a strategy chosen by the **`dialect`** plugin option (passed to both
`protoc-gen-ent` and `protoc-gen-storage`):

| `dialect` | Strategy | Mechanism |
|---|---|---|
| `postgres` (default), `sqlite` | **Partial unique index** | the composite carries `WHERE delete_time IS NULL` (ent) / `WHERE deleted_at IS NULL` (GORM), so only live rows participate in uniqueness. No extra column. |
| `mysql` | **Discriminator column** | MySQL has no partial indexes, so a `soft_delete_key` column joins the composite as its trailing column — `""` while the row is live (uniqueness holds among live rows), a unique marker once soft-deleted (so it never blocks re-creation). The framework stamps it on soft-delete and clears it on undelete; no consumer code. |

Set it in `buf.gen.yaml` to match your **production** database (dev on SQLite still works either way):

```yaml
plugins:
  - local: protoc-gen-storage
    opt: [module=…, dialect=mysql]   # omit for postgres/sqlite (the default)
  - local: protoc-gen-ent
    opt: [dialect=mysql]
```

The behavior is identical across backends and dialects; only the physical schema differs. A per-tenant
`unique` field on a resource **without** soft-delete is unaffected (a plain composite unique index, as
before).

A sibling marker, `google.protobuf.Timestamp expire_time = N [(google.api.field_behavior) = OUTPUT_ONLY]`,
additionally emits a `PurgeExpired(ctx, before)` method that hard-deletes rows past their TTL.
`expire_time` is OUTPUT_ONLY, so your **Create handler stamps it** (e.g.
`b.ExpireTime = timestamppb.New(time.Now().Add(ttl))` before `repo.Create`); the generated `toModel`
carries it to the `expire_time` column, and `PurgeExpired(ctx, time.Now())` reaps the rows whose stamp
has passed.

{{< callout type="info" >}}
**SQLite + time zones:** `toModel` stores `expire_time` in UTC, and the generated `PurgeExpired`
normalizes its cutoff to UTC (`before.UTC()`). On SQLite, time columns are stored as TZ-suffixed
TEXT and compared textually, so a UTC-stored value would not match a local-zone cutoff — without
this normalization `PurgeExpired(time.Now())` could silently reap nothing. You may still pass a UTC
cutoff explicitly; both work.
{{< /callout >}}

### ETag (AIP-154 / optimistic concurrency)

Declare a server-managed ETag field on the resource:

```protobuf
string etag = N [(google.api.field_behavior) = OUTPUT_ONLY];
```

`protoc-gen-storage` then **stamps a fresh opaque token on every Create and Update** and **surfaces
it on every read** (`fromModel` copies the stored `etag` column onto the proto). A `Get` therefore
returns a stable token a client echoes as `If-Match`; the token changes on the next write, so a
stale `If-Match` is rejected with a 412 (see [middleware → etag](../middleware/#etag--412-preconditions)
for the handler pattern). The token is opaque — clients must not parse it.

On the **ent** backend you get the same behavior: `protoc-gen-ent` adds the generated `entrepo.EtagMixin`,
which supplies the `etag` column **and** a mutation hook that stamps a fresh `etag.New()` token on every
Create/Update — automatically, with no consumer code on the write path. The **generated**
`fromEnt<Resource>` projection surfaces it on reads (`p.Etag = e.Etag`), so the AIP-154 round-trip
works with no consumer code on either path.

The If-Match precondition comparison is the same documented handler pattern on both backends (see
[middleware → etag](../middleware/#etag--412-preconditions)).

### Secret fields

A field marked `(infoblox.field.v1.opts) = {secret: true}` does **not** get a plaintext column.
Instead `protoc-gen-storage` emits two columns:

```go
KeyValueHash   string `gorm:"column:key_value_hash;index"` // for lookup
KeyValueCipher string `gorm:"column:key_value_cipher"`       // for recovery
```

The constructor then requires an `Encryptor`:

```go
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
```

`Create`/`Update` hash and encrypt the value; a `LookupBy<Field>Hash` method is emitted for each
secret field so you can find a record by the hash of a presented value.

### Tenant scoping

If a message has an `account_id` field, every `Get`/`List`/`Update`/`Delete`/`LookupBy*Hash`
query adds an `account_id = ?` clause from `middleware.TenantIDFromContext(ctx)`:

```go
tenantID := middleware.TenantIDFromContext(ctx)
q := r.db.WithContext(ctx).Where("id = ?", key)
if tenantID != "" {
    q = q.Where("account_id = ?", tenantID)
}
```

{{< callout type="info" >}}
Because the generated code imports `gorm.io/gorm`, generate it into a module that has gorm as a
dependency — never the SDK's own module. The SDK keeps gorm out of its `go.mod` this way.
{{< /callout >}}

## protoc-gen-ent

Generates an ent schema for each **resource** message. Run `go generate ./ent` to produce the
type-safe client from the schema. The ent shape enforces tenant scoping and secret-field handling
through ent's **privacy layer** and **hooks** (applied by a generated mixin), so the invariants hold
even for ad-hoc graph traversals — not just CRUD.

A message is a **resource** (and gets a schema) when it declares an `id` field — the same rule
`protoc-gen-storage` uses. Request/response wrappers and other transport types without an `id`
(for example a consumer-declared LRO `Operation`) are **skipped**, so they get no ent schema or
batch wrapper.

`protoc-gen-ent` generates the ent **schema** (under `ent/schema/`), a batch wrapper, **and the
repository adapter itself** (`<resource>_repo.ent.go`) — so you write **no** hand-written ent wiring.
The generated `New<Resource>EntRepository` fills the six `entrepo.EntRepository[T, K]` closures
(Get/List/Create/Update/Delete/Undelete) over the ent client, with tenant guards on mutations,
`persistence.ConstraintError` classification, `ent.IsNotFound → persistence.ErrNotFound` mapping, the
secret hash/cipher block, AIP-160 filtering from the generated `<Resource>EntColumns` maps, a
conditional set for `belongs_to` foreign keys, and the `fromEnt<Resource>` projection. A
`LookupBy<Field>Hash` is emitted per secret field. Just construct it:

```go
repo := apikeyv1.NewAPIKeyEntRepository(client, enc) // persistence.Repository[*APIKey, string]
```

**Customization (the owned seam).** For computed/derived values or columns the generator can't infer,
register the generated hooks from your own (regen-safe) file — the adapter calls them when set, so
re-running codegen never disturbs your custom logic:

```go
func init() {
    apikeyv1.FromEntAPIKeyCustom  = func(e *ent.APIKey, p *apikeyv1.APIKey)     { /* read projection */ }
    apikeyv1.ToEntAPIKeyOnCreate  = func(p *apikeyv1.APIKey, b *ent.APIKeyCreate)   { /* extra write columns */ }
    apikeyv1.ToEntAPIKeyOnUpdate  = func(p *apikeyv1.APIKey, u *ent.APIKeyUpdateOne) { /* extra write columns */ }
}
```

**Fail-closed coverage.** If a resource field has no deterministic mapping — a nested
non-relationship message, a repeated non-relationship field, an enum, or a non-string map —
generation **fails**, naming the field and the remedy, rather than silently dropping it. Make it a
relationship, give it a scalar storage type, or mark it `OUTPUT_ONLY`.

**Multi-surface (reserved).** The `(infoblox.storage.v1.model)` message option binds a resource to a
backing storage model so several API surfaces can project one stored entity. The annotation is the
locked contract today; the cross-message surface codegen is forthcoming (a value other than the
message's own name is rejected until then).

### Relationships in ent

For a parent/child pair — a `has_many` on the parent and a `belongs_to` on the child (the
[natural AIP shape](../../concepts/annotations/#relationships)) — the plugin emits a single ent
edge as a proper **inverse pair**, referencing each related schema by its **singular** type:

```go
// parent: Fleet has_many Vehicle
func (Fleet) Edges() []ent.Edge {
    return []ent.Edge{
        edge.To("vehicles", Vehicle.Type),
    }
}

// child: Vehicle belongs_to Fleet, with a scalar fleet_id FK
func (Vehicle) Fields() []ent.Field {
    return []ent.Field{
        field.String("id").StorageKey("id").Immutable(),
        field.String("fleet_id").Optional(),
    }
}
func (Vehicle) Edges() []ent.Edge {
    return []ent.Edge{
        edge.From("fleet", Fleet.Type).Ref("vehicles").Unique().Field("fleet_id"),
    }
}
```

The child's `.Field("fleet_id")` **binds the edge's foreign key to the scalar `fleet_id` column**,
so ent generates a single `SetFleetID` setter — the scalar field and the association share one
column rather than colliding. The FK stays a first-class, queryable scalar. A `belongs_to` whose
parent does **not** declare a reciprocal `has_many` is emitted as a self-contained
`edge.To(...).Unique()` (also binding the scalar FK when present).

A complete, buildable two-resource example (schema, ent client, wiring, and an edge-traversal test)
lives in `testdata/fleet/`.

{{< callout type="warning" >}}
**Standing up the ent client — gotchas:**
- In a **consumer module**, give `protoc-gen-ent` a bare `out: .` (it writes `ent/schema/...`). Do
  **not** add the `opt: module=...` used for the other plugins — ent rejects it
  (`generated file does not match prefix`).
- **Cold-start order matters.** `buf generate` writes the schema *and* generated files that import
  the not-yet-generated ent *client* packages (`<module>/ent/<resource>`): the `*.batch.ent.go` batch
  wrappers and the `ent/*_filter.ent.go` tenant / soft-delete filterers. The generated **schema** also
  imports `persistence/entrepo` (for `TenantMixin` / `SoftDeleteMixin`), and `entc` *compiles the schema
  package* during `go generate`, so `entrepo`'s transitive deps (grpc, protobuf) must already be in
  `go.sum` before you generate. The two hazards: a plain `go mod tidy` may choke on the not-yet-existing
  `<module>/ent/<resource>` imports, and `go generate ./ent` fails with `missing go.sum entry … imported
  by …/persistence/entrepo` if those deps aren't seeded yet. Break the deadlock once, **in this order**:

  1. Pin the ent codegen tool so it stays in `go.mod` across future tidies — add a `tools.go`:
     ```go
     //go:build tools

     package tools

     import _ "entgo.io/ent/cmd/ent"
     ```
  2. Seed `go.sum` with the ent codegen tool **and** the SDK packages the generated schema, filterers,
     and batch wrappers import — `go get` resolves them and their transitive deps **without building
     your module** (so it does not choke on the not-yet-generated client packages):
     ```
     go get entgo.io/ent/cmd/ent \
            github.com/infobloxopen/devedge-sdk/persistence/entrepo \
            github.com/infobloxopen/devedge-sdk/middleware
     ```
  3. `go generate ./ent` — `entc` can now compile the schema (its `entrepo` deps are in `go.sum`) and
     produces the client, so the `<module>/ent/<resource>` packages the wrappers and filterers import
     now exist.
  4. `go mod tidy` — everything resolves now that the client is generated.

  After this, regenerating is just `buf generate` → `go generate ./ent`. `testdata/fleet/tools.go`
  shows the pin. (If you skip step 2, `go generate` aborts on the missing `entrepo` go.sum entries; if
  you run a bare `go mod tidy` before generating, it may fail to resolve your own not-yet-generated
  `<module>/ent/<resource>` packages — step 2 avoids both.)

**Testing the ent client (SQLite driver name).** ent's SQLite dialect is `"sqlite3"`
(`dialect.SQLite`), but the pure-Go driver `modernc.org/sqlite` registers itself under the name
`"sqlite"` — so `enttest.Open(t, "sqlite3", …)` fails with `sql: unknown driver "sqlite3"` unless you
register the alias. Do it once in your test package, and turn on foreign keys in the DSN:

```go
import (
    "database/sql"
    "database/sql/driver"

    _ "modernc.org/sqlite" // registers driver name "sqlite"
)

func init() {
    for _, n := range sql.Drivers() {
        if n == "sqlite3" {
            return // already present (e.g. mattn/go-sqlite3 pulled in transitively)
        }
    }
    db, _ := sql.Open("sqlite", ":memory:")
    drv := db.Driver()
    _ = db.Close()
    sql.Register("sqlite3", drv.(driver.Driver))
}

// client := enttest.Open(t, "sqlite3", "file:ent?mode=memory&_pragma=foreign_keys(1)")
```

If the same module also pulls `github.com/mattn/go-sqlite3` (e.g. via `gorm.io/driver/sqlite` for a
GORM backend), it already registers `"sqlite3"` — the guard above skips the alias — and it reads the
foreign-key pragma as `_fk=1`, so use a DSN that satisfies both drivers:
`file:ent?mode=memory&_pragma=foreign_keys(1)&_fk=1`. `testdata/fleet/fleetv1/sqlite_test.go` is the
canonical shim.
{{< /callout >}}

{{< callout type="error" >}}
**Your server `main` MUST blank-import the generated `ent/runtime` — this is security-relevant.** entc
emits an `ent/runtime/runtime.go` whose `init()` installs the schema's field validators **and the
`TenantMixin` / `SoftDeleteMixin` query interceptors** (the ones that call the generated
`WhereAccountID` / `WhereDeleteTimeIsNil` filterers). The generated `ent/client.go` does **not** import
it, so unless you blank-import it in every non-test entrypoint the client compiles but:

- panics with a nil-pointer on the first write (a nil field validator), and
- runs **no tenant / soft-delete scoping** — the generated filterers exist but are never called.

`enttest` imports it for you, so tests pass and the gap only shows up when you run a real server. Add it
to your server `main` (and any other non-test entrypoint):

```go
import _ "<your-module>/ent/runtime" // installs mixin validators + tenant/soft-delete interceptors
```
{{< /callout >}}

## Putting them in `buf.gen.yaml`

```yaml {filename="buf.gen.yaml"}
version: v2
plugins:
  - local: protoc-gen-go
  - local: protoc-gen-go-grpc
  - local: protoc-gen-devedge-authz
  - local: protoc-gen-svc
  - local: protoc-gen-storage
  - local: protoc-gen-grpc-gateway
  - local: protoc-gen-ent
```

See [Define a service](../../guides/define-a-service/) for the complete configured example.
