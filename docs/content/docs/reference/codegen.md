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
for the handler pattern). The token is opaque — clients must not parse it. On the **ent** backend the
`etag` field is not auto-stamped (compute it in your ent wiring if needed).

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

`protoc-gen-ent` generates the ent **schema** (under `ent/schema/`) and a batch wrapper; it does
**not** generate the adapter that satisfies the neutral `persistence.Repository` seam. You write a
thin one using the SDK's generic `entrepo.EntRepository[T, K]`, supplying closures for
Get/List/Create/Update/Delete/Undelete over the ent client, behind this constructor:

```go
func NewAPIKeyEntRepository(client *ent.Client, enc secret.Encryptor) persistence.Repository[*APIKey, string]
```

A complete, copy-able adapter (tenant guards on mutations, secret hash/cipher on create, soft-delete
via `delete_time`) lives in the apikey example at `testdata/apikey/apikeyv1/ent_wiring.go`.

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
  wrappers and the `ent/*_filter.ent.go` tenant / soft-delete filterers. In a fresh module a plain
  `go mod tidy` therefore **fails** — it treats `<module>/ent/<resource>` as a remote dependency and
  reports `Repository not found` for your own module path, aborting before it can pull the ent codegen
  toolchain that `go generate ./ent` then needs in `go.sum`. Break the deadlock once, in this order:

  1. Pin the ent codegen tool so it stays in `go.mod` across future tidies — add a `tools.go`:
     ```go
     //go:build tools

     package tools

     import _ "entgo.io/ent/cmd/ent"
     ```
  2. Pull the codegen toolchain into `go.sum` **without** a full `go mod tidy` (which would choke on
     the not-yet-generated client packages above) — `go get` resolves the tool and its deps without
     building your module:
     ```
     go get entgo.io/ent/cmd/ent
     ```
  3. `go generate ./ent` — this produces the client, so the `<module>/ent/<resource>` packages the
     wrappers and filterers import now exist.
  4. `go mod tidy` — everything resolves now that the client is generated.

  After this, regenerating is just `buf generate` → `go generate ./ent`. `testdata/fleet/tools.go`
  shows the pin.

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
