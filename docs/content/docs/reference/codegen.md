---
title: codegen
weight: 6
---

The SDK ships four `protoc` plugins that generate running Go code from your proto definitions. You invoke them through `buf generate`. Install them with:

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

This plugin reads `(infoblox.authz.v1.rule)` method annotations and emits a `[]authz.MethodRule` table as a checked-in file. The result is equivalent to what `authzpb.RulesFromGlobal()` would produce by reflection, but is verified at compile time.

The generated `Register<Service>` (see [protoc-gen-svc](#protoc-gen-svc) below) calls `server.AddRules(<Service>AuthzRules...)` for you, so a service's rules are contributed to the server at registration time. `server.Config.Rules` is an optional additive override — you can merge extra rules in when you need to, but the generated path does not require it. The boot-time completeness gate runs at `server.Serve` over the accumulated rule set (fail-closed); see [server → Serve](../server/#serve).

The plugin emits one `<Service>AuthzRules` variable per `service` declaration, plus an `AllAuthzRules` that concatenates them for the whole file:

```go
// Generated:
var FooServiceAuthzRules = []authz.MethodRule{ /* ... */ }
var FooSummaryServiceAuthzRules = []authz.MethodRule{ /* ... */ }
var AllAuthzRules = slices.Concat(FooServiceAuthzRules, FooSummaryServiceAuthzRules)
```

Register each service (or use the `…WithRepository` one-liner) and its rules are contributed automatically — there is nothing to pass to `Config.Rules`. `AllAuthzRules` is useful when you want the whole file's rules in one reference, for example to seed a permission catalog.

## protoc-gen-svc

This plugin generates the server-package wiring for each service. For a `WidgetService` over a `Widget` resource it emits:

- **`Register<Service>(s *server.Server, impl <Service>Server) error`** — records the service's
  methods on the server, contributes `<Service>AuthzRules` via `server.AddRules`, registers `impl`
  on the gRPC server, and wires the HTTP/JSON gateway. The boot-time authz completeness gate runs
  later, at `server.Serve`, over the accumulated rule set (fail-closed).
- **A generated default CRUD handler, `<Service>CRUDHandler`**. It embeds `Unimplemented<Service>Server` and holds a `persistence.Repository[*<Resource>, string]`, with one method per detected AIP standard method delegating to the repository:

  | Detected RPC shape | Generated body |
  |---|---|
  | `Create<R>` (request carries the resource) | `repo.Create(ctx, req.Get<R>())` |
  | `Get<R>` / `Delete<R>` (request keyed by `id` **or** an AIP-122 `name`) | `repo.Get`/`repo.Delete` — when keyed by `name`, parses it via `Parse<R>Name` |
  | `List<R>s` (paging + optional `filter`/`order_by`/`show_deleted`) | `repo.List(persistence.ListOptions{...})` |
  | `Update<R>` (resource + `update_mask`) | `repo.Update(res.Id, res, update_mask...)` |
  | `Undelete<R>` (soft-delete resources) | `repo.Undelete` |

  Detection is by request/response shape, not name alone, and tolerates extra optional fields. An RPC matching no standard shape (for example an AIP-136 custom method) is left `Unimplemented` and is never silently mis-handled. Tenant stamping is the repository's job — it stamps `account_id` from context on create. The interceptor chain applies `read_mask`/`field_mask`, so the handler does neither.

- **`New<Service>Handler(repo) *<Service>CRUDHandler`** — returns the default handler so you can embed or wrap it before registering via `Register<Service>`.
- **`Register<Service>WithRepository(s *server.Server, repo) error`** — the one-call CRUD path
  (`New<Service>Handler` + `Register<Service>`). A pure-CRUD service needs nothing else:

  ```go
  repo := myv1.NewWidgetRepository(db) // or NewWidgetEntRepository(client)
  if err := myv1.RegisterWidgetServiceWithRepository(s, repo); err != nil { /* ... */ }
  // ... s.Serve(ctx)
  ```

### Overriding a method (escape hatch)

The generated handler is marked `DO NOT EDIT`. To add custom or non-CRUD logic, embed `<Service>CRUDHandler` in your own type and redefine only the methods you change. The remaining methods still come from the generated defaults, and any custom RPC the generator left `Unimplemented` is now served. Regenerating codegen does not disturb your override because it lives in your file:

```go
type handler struct {
    myv1.WidgetServiceCRUDHandler // Get/List/Update/Delete come from here
}

// Override Create to add custom logic; everything else is the generated default.
func (h *handler) CreateWidget(ctx context.Context, req *myv1.CreateWidgetRequest) (*myv1.Widget, error) {
    if req.Widget.Id == "" { req.Widget.Id = uuid.New().String() }
    return h.Repo.Create(ctx, req.Widget)
}

// ArchiveWidget is an AIP-136 custom method the generated handler left Unimplemented.
func (h *handler) ArchiveWidget(ctx context.Context, req *myv1.ArchiveWidgetRequest) (*myv1.ArchiveWidgetResponse, error) { /* ... */ }

h := &handler{}
h.WidgetServiceCRUDHandler.Repo = repo
_ = myv1.RegisterWidgetService(s, h) // plain Register, with your overriding handler
```

For a fully custom (non-CRUD) service, implement the bare `<Service>Server` interface and use `Register<Service>(s, custom)` directly.

In a scaffolded service the `s *server.Server` you register on is `app.Server`, the shared server a [`servicekit`](../servicekit/) module is handed. [Add a custom method or second resource](../../how-to/model-and-persist/custom-methods/) walks this override through the module and host.

## protoc-gen-storage

This plugin generates a GORM-backed `Repository` for each message. For a message named `APIKey` it emits:

- **`APIKeyModel`** — the GORM model. Standard columns plus `ETag`, `CreatedAt`, `UpdatedAt` — and, only for resources that opt into soft-delete (see below), a `DeletedAt` column of type `gorm.DeletedAt`. The `APIKey` example opts in, so its model has `DeletedAt`. The `ETag` column is populated only when the resource declares an `etag` field (see [ETag](#etag-aip-154--optimistic-concurrency) below).
- **`toModel_APIKey` / `fromModel_APIKey`** — converters. They skip repeated fields (TODO: JSONB), nested messages (TODO: serialization), and secret fields.
- **`APIKeyRepository`** + **`NewAPIKeyRepository`** — `Get`, `List`, `Create`, `Update`, `Delete`, satisfying `persistence.Repository[*APIKey, string]` (a compile-time `var _` check is emitted).

{{< callout type="info" >}}
Because the generated code imports `gorm.io/gorm`, generate it into a module that has gorm as a dependency — never the SDK's own module. The SDK keeps gorm out of its `go.mod` this way.
{{< /callout >}}

### Soft-delete

Soft-delete is opt-in per resource, following AIP-148/149. A message becomes soft-deletable when it declares a server-managed `delete_time` timestamp:

```protobuf
google.protobuf.Timestamp delete_time = 7 [(google.api.field_behavior) = OUTPUT_ONLY];
```

The field name must be `delete_time`, the type `google.protobuf.Timestamp`, and it must be `OUTPUT_ONLY` — the framework sets it, not the client. When present, `protoc-gen-storage` emits the full soft-delete shape:

- a `DeletedAt` column (`gorm.DeletedAt`, indexed) on the model;
- `Delete` performs a soft delete (stamps `deleted_at`) instead of a physical row delete;
- `List` excludes soft-deleted rows unless `ListOptions.ShowDeleted` is set;
- `Undelete` (AIP-149) clears `deleted_at` and restores the row.

Without a `delete_time` field a resource is hard-delete by default: `Delete` physically removes the row, `List` has no deleted-row filter, and `Undelete` is a stub that always returns `persistence.ErrNotFound` so the `Repository` interface stays uniform. Adding the `delete_time` field is the only switch — there is no separate annotation or option.

#### Soft-delete with unique fields and the `dialect` option

When a resource is both soft-deletable and has a per-tenant `unique` field (for example `source_ref`), the per-tenant composite unique constraint `(account_id, <field>)` must not let a soft-deleted row keep the key reserved. You must be able to create a fresh resource with the same value after the old one is soft-deleted, and `Undelete` must correctly fail with a conflict if the key was taken in the meantime. The codegen handles this automatically, using a strategy chosen by the `dialect` plugin option (passed to both `protoc-gen-ent` and `protoc-gen-storage`):

| `dialect` | Strategy | Mechanism |
|---|---|---|
| `postgres` (default), `sqlite` | Partial unique index | the composite carries `WHERE delete_time IS NULL` (ent) / `WHERE deleted_at IS NULL` (GORM), so only live rows participate in uniqueness. No extra column. |
| `mysql` | Discriminator column | MySQL has no partial indexes, so a `soft_delete_key` column joins the composite as its trailing column — `""` while the row is live (uniqueness holds among live rows), a unique marker once soft-deleted (so it never blocks re-creation). The framework stamps it on soft-delete and clears it on undelete; no consumer code is required. |

Set the option in `buf.gen.yaml` to match your production database (dev on SQLite still works either way):

```yaml
plugins:
  - local: protoc-gen-storage
    opt: [module=…, dialect=mysql]   # omit for postgres/sqlite (the default)
  - local: protoc-gen-ent
    opt: [dialect=mysql]
```

The behavior is identical across backends and dialects; only the physical schema differs. A per-tenant `unique` field on a resource without soft-delete is unaffected — it gets a plain composite unique index.

A sibling marker, `google.protobuf.Timestamp expire_time = N [(google.api.field_behavior) = OUTPUT_ONLY]`, additionally emits a `PurgeExpired(ctx, before)` method that hard-deletes rows past their TTL. `expire_time` is `OUTPUT_ONLY`, so your Create handler stamps it (for example `b.ExpireTime = timestamppb.New(time.Now().Add(ttl))` before `repo.Create`). The generated `toModel` carries it to the `expire_time` column, and `PurgeExpired(ctx, time.Now())` removes rows whose stamp has passed.

{{< callout type="info" >}}
**SQLite and time zones:** `toModel` stores `expire_time` in UTC, and the generated `PurgeExpired` normalizes its cutoff to UTC (`before.UTC()`). On SQLite, time columns are stored as TZ-suffixed TEXT and compared textually, so a UTC-stored value would not match a local-zone cutoff — without this normalization `PurgeExpired(time.Now())` could silently reap nothing. You may still pass a UTC cutoff explicitly; both work.
{{< /callout >}}

### ETag (AIP-154 / optimistic concurrency)

Declare a server-managed ETag field on the resource:

```protobuf
string etag = N [(google.api.field_behavior) = OUTPUT_ONLY];
```

`protoc-gen-storage` then stamps a fresh opaque token on every Create and Update, and surfaces it on every read (`fromModel` copies the stored `etag` column onto the proto). A `Get` therefore returns a stable token a client echoes as `If-Match`; the token changes on the next write, so a stale `If-Match` is rejected with a 412 (see [middleware → ETag and preconditions](../middleware/#etag-and-preconditions) for the handler pattern). The token is opaque — clients must not parse it.

On the ent backend you get the same behavior. `protoc-gen-ent` adds the generated `entrepo.EtagMixin`, which supplies the `etag` column and a mutation hook that stamps a fresh `etag.New()` token on every Create and Update automatically, with no consumer code on the write path. The generated `fromEnt<Resource>` projection surfaces it on reads (`p.Etag = e.Etag`), so the AIP-154 round-trip works with no consumer code on either path.

The If-Match precondition comparison follows the same handler pattern on both backends (see [middleware → ETag and preconditions](../middleware/#etag-and-preconditions)).

### Secret fields

A field marked `(infoblox.field.v1.opts) = {secret: true}` does not get a plaintext column. Instead `protoc-gen-storage` emits two columns:

```go
KeyValueHash   string `gorm:"column:key_value_hash;index"` // for lookup
KeyValueCipher string `gorm:"column:key_value_cipher"`       // for recovery
```

The constructor then requires an `Encryptor`:

```go
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
```

`Create`/`Update` hash and encrypt the value. A `LookupBy<Field>Hash` method is emitted for each secret field so you can find a record by the hash of a presented value.

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

## protoc-gen-ent

This plugin generates an ent schema for each resource message (a message with an `id` field). Run `go generate` on the directory the schema landed in to produce the type-safe ent client. With the scaffold's `out: gen` setting the command is `go generate ./gen/ent`; use `go generate ./ent` only if you set `out: .`.

{{< callout type="warning" >}}
**Point `go generate` at the correct directory.** Pointing it at the wrong directory silently no-ops, leaving a stale or missing client.
{{< /callout >}}

The ent shape enforces tenant scoping and secret-field handling through ent's privacy layer and hooks (applied by a generated mixin), so the invariants hold even for ad-hoc graph traversals — not just CRUD.

A message is treated as a resource when it declares an `id` field — the same rule `protoc-gen-storage` uses. Request/response wrappers and other transport types without an `id` (for example a consumer-declared LRO `Operation`) are skipped and receive no ent schema or batch wrapper.

`protoc-gen-ent` generates the ent schema (under `ent/schema/`), a batch wrapper, and the repository adapter itself (`<resource>_repo.ent.go`), so you write no hand-written ent wiring. The generated `New<Resource>EntRepository` fills the six `entrepo.EntRepository[T, K]` closures (Get/List/Create/Update/Delete/Undelete) over the ent client, with:

- tenant guards on mutations;
- `persistence.ConstraintError` classification;
- `ent.IsNotFound → persistence.ErrNotFound` mapping;
- the secret hash/cipher block;
- AIP-160 filtering from the generated `<Resource>EntColumns` maps;
- a conditional set for `belongs_to` foreign keys;
- the `fromEnt<Resource>` projection.

A `LookupBy<Field>Hash` is emitted per secret field. Construct the repository with:

```go
repo := apikeyv1.NewAPIKeyEntRepository(client, enc) // persistence.Repository[*APIKey, string]
```

### Customization

For computed or derived values that the generator cannot infer, register the generated hooks from your own regen-safe file. The adapter calls them when set, so re-running codegen does not disturb your custom logic:

```go
func init() {
    apikeyv1.FromEntAPIKeyCustom  = func(e *ent.APIKey, p *apikeyv1.APIKey)     { /* read projection */ }
    apikeyv1.ToEntAPIKeyOnCreate  = func(p *apikeyv1.APIKey, b *ent.APIKeyCreate)   { /* extra write columns */ }
    apikeyv1.ToEntAPIKeyOnUpdate  = func(p *apikeyv1.APIKey, u *ent.APIKeyUpdateOne) { /* extra write columns */ }
}
```

### Fail-closed field coverage

If a resource field has no deterministic mapping — a nested non-relationship message, a repeated non-relationship field, an enum, or a non-string map — generation fails and names the field and the remedy, rather than silently dropping it. To resolve this, make the field a relationship, give it a scalar storage type, or mark it `OUTPUT_ONLY`.

### Multi-surface

The `(infoblox.storage.v1.model)` message option (from `github.com/infobloxopen/apis/proto/infoblox/storage/v1`) binds a resource message to a backing storage model so several API surfaces can project one stored entity. When absent or equal to the message's own name, the message is an ordinary single-table resource. When the value names a different message, this message is a surface (a projection) over that owner's model:

```proto
import "infoblox/storage/v1/storage.proto";

message Coupon { string id = 1; string account_id = 2; string code = 3; /* … */ }

message CouponSummary {                                 // a read projection over Coupon
  option (infoblox.storage.v1.model) = "Coupon";
  string id = 1; string account_id = 2; string code = 3; // a subset of Coupon's fields
}
```

Both `protoc-gen-ent` and `protoc-gen-storage` then emit, for the surface, a `New<Surface>…Repository` plus a projection over the owner's type — and no table of its own (no ent schema, no GORM model struct). One model can have N repositories/projections. Mutation guards (tenant scope, soft-delete, undelete) follow the owner; the written/projected columns follow the surface's own fields.

An owner and its surfaces are fully generated on both backends with no hand-written adapter. The apikey fixture's `APIKeySummary` surface (a tenant-scoped projection over `APIKey`) round-trips owner-write → surface-read on ent and GORM with no hand-written persistence code (`testdata/apikey/.../multisurface_test.go`).

{{< callout type="warning" >}}
**Invalid surface declarations are rejected at generation time.** Generation fails if a surface names a missing or non-base owner, declares a relationship, projects a field the owner has no column for (or with a mismatched type, secret, or output classification), or omits `account_id` for a tenant-scoped model. A surface field set must be a subset of its owner's.
{{< /callout >}}

{{< callout type="info" >}}
**A surface projects columns; it cannot declare a derived field.** A rollup such as a total or a count has no column on the owner to project, so a summary surface cannot carry one. Return the rollup in the custom RPC's response message, computed in the handler. On the ent backend you can also fill a derived field from the read hook `FromEnt<Owner>Custom` (see [Customization](#customization)).
{{< /callout >}}

### Resource names on ent (AIP-122)

When a resource carries a `(google.api.resource)` pattern and an `OUTPUT_ONLY` `name` field, the ent backend derives `name` from `id` and never stores it. The generated ent schema omits the `name` column, the adapter never writes it, and the generated `fromEnt<R>` projection recomputes it on every read via `Format<R>Name(e.ID)`. A `Get`, `List`, `Create`, or `Update` response always carries the resource name with no consumer code.

The plugin also emits the same package-level helpers the GORM backend does:

```go
const <R>NamePattern = "<resources>/{<resource>}"
func Format<R>Name(id string) string          // id → "resources/abc123"
func Parse<R>Name(name string) (string, error) // "resources/abc123" → id
```

{{< callout type="info" >}}
**Using ent and GORM backends in the same package.** When `protoc-gen-storage` also runs into the same package — as in a both-backends test fixture — pass the ent plugin `opt: with_storage=true` so it does not re-emit these helpers. The storage plugin already owns them, and the ent `fromEnt<R>` calls the storage-emitted `Format<R>Name`. An ent-only service (the normal scaffold) leaves this option off, and the ent plugin emits the helpers itself.
{{< /callout >}}

### Relationships in ent

For a parent/child pair — a `has_many` on the parent and a `belongs_to` on the child (the [natural AIP shape](../../concepts/annotations/#orm-relationships)) — the plugin emits a single ent edge as a proper inverse pair, referencing each related schema by its singular type:

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

The child's `.Field("fleet_id")` binds the edge's foreign key to the scalar `fleet_id` column, so ent generates a single `SetFleetID` setter — the scalar field and the association share one column rather than colliding. The FK stays a first-class, queryable scalar. A `belongs_to` whose parent does not declare a reciprocal `has_many` is emitted as a self-contained `edge.To(...).Unique()`, also binding the scalar FK when present.

A complete, buildable two-resource example (schema, ent client, wiring, and an edge-traversal test)
lives in `testdata/fleet/`.

{{< callout type="warning" >}}
**Standing up the ent client — gotchas:**
- **`protoc-gen-ent` takes NO `module=` opt** (unlike the other plugins) — ent rejects it (`generated file does not match prefix`). It derives its output directory from the proto's Go package and writes `<out>/ent/schema/...` as a sibling of the proto package. Set `out:` to wherever you want generated code rooted: the `devedge-sdk new service` scaffold uses `out: gen` (so the schema lands in `gen/ent/`, next to `gen/<svc>v1`). (`out: .` also works — it roots generated code at the module top instead of under `gen/`.)
- **Cold-start order matters.** `buf generate` writes the schema and generated files that import the not-yet-generated ent client packages (`<module>/ent/<resource>`): the `*.batch.ent.go` batch wrappers and the `ent/*_filter.ent.go` tenant/soft-delete filterers. The generated schema also imports `persistence/entrepo` (for `TenantMixin` / `SoftDeleteMixin`), and `entc` compiles the schema package during `go generate`, so `entrepo`'s transitive deps (grpc, protobuf) must already be in `go.sum` before you generate. Break the cold-start deadlock once, in this order:

  1. Pin the ent codegen tool so it stays in `go.mod` across future tidies — add a `tools.go`:
     ```go
     //go:build tools

     package tools

     import _ "entgo.io/ent/cmd/ent"
     ```
  2. Seed `go.sum` with the ent codegen tool and the SDK packages the generated schema, filterers, adapter, and batch wrappers import. Use `go get`, not `go mod tidy -e` — `tidy` builds the module, so on a fresh clone it hits the not-yet-generated `<module>/ent/<resource>` imports and fails with `cannot find module providing package <module>/gen/ent/<resource>`. `go get` of the exact packages does not build the module, so the cold-start stays clean:
     ```
     go get entgo.io/ent/cmd/ent \
            github.com/infobloxopen/devedge-sdk/persistence/entrepo \
            github.com/infobloxopen/devedge-sdk/middleware \
            github.com/infobloxopen/devedge-sdk/persistence/resourcename
     ```
     (`persistence/resourcename` backs the generated AIP-122 `Format<R>Name` helper; including it is harmless even when a resource has no name pattern.) This `go get` sequence is what `make generate` runs for you in a scaffolded service.
  3. `go generate ./gen/ent` (or `./ent` if you used `out: .`) — `entc` can now compile the schema (its `entrepo` deps are in `go.sum`) and produces the client, so the `<module>/ent/<resource>` packages the wrappers and filterers import now exist.
  4. `go mod tidy` — everything resolves now that the client is generated.

  After this, regenerating is just `buf generate` → `go generate ./gen/ent`. `testdata/fleet/tools.go` shows the pin. If you skip step 2, `go generate` aborts on the missing `entrepo` go.sum entries; if you run a bare `go mod tidy` before generating, it may fail to resolve your own not-yet-generated `<module>/ent/<resource>` packages — step 2 avoids both.

**Testing the ent client (SQLite driver name).** ent's SQLite dialect is `"sqlite3"` (`dialect.SQLite`), but the pure-Go driver `modernc.org/sqlite` registers itself under the name `"sqlite"` — so `enttest.Open(t, "sqlite3", …)` fails with `sql: unknown driver "sqlite3"` unless you register the alias. Do it once in your test package, and turn on foreign keys in the DSN:

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

If the same module also pulls `github.com/mattn/go-sqlite3` (for example via `gorm.io/driver/sqlite` for a GORM backend), it already registers `"sqlite3"` — the guard above skips the alias — and it reads the foreign-key pragma as `_fk=1`, so use a DSN that satisfies both drivers: `file:ent?mode=memory&_pragma=foreign_keys(1)&_fk=1`. `testdata/fleet/fleetv1/sqlite_test.go` is the canonical shim.
{{< /callout >}}

{{< callout type="error" >}}
**Your server `main` must blank-import the generated `ent/runtime` — this is security-relevant.** `entc` emits an `ent/runtime/runtime.go` whose `init()` installs the schema's field validators and the `TenantMixin` / `SoftDeleteMixin` query interceptors (the ones that call the generated `WhereAccountID` / `WhereDeleteTimeIsNil` filterers). The generated `ent/client.go` does not import it, so unless you blank-import it in every non-test entrypoint — you MUST do this — the client compiles but:

- panics with a nil-pointer on the first write (a nil field validator), and
- runs no tenant or soft-delete scoping — the generated filterers exist but are never called.

`enttest` imports it for you, so tests pass and the gap only appears when you run a real server. Add it to your server `main` (and any other non-test entrypoint):

```go
import _ "<your-module>/ent/runtime" // installs mixin validators + tenant/soft-delete interceptors
```
{{< /callout >}}

## Configuring buf.gen.yaml

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

A real service uses one storage backend — `protoc-gen-storage` (GORM) or `protoc-gen-ent`, not both (ent replaces storage). The combined list above shows all available plugins; the `devedge-sdk new service` scaffold emits one or the other.

See [Define a service](../../how-to/model-and-persist/define-a-service/) for a complete configured example.

{{< callout type="info" >}}
**Expected `go_package` mismatch warning (ent scaffold + apx).** On the ent path the proto's `go_package` is a single segment (`gen/<svc>v1`, no `module=` opt) so the generated `ent/` package compiles as a sibling of the proto package. apx, however, derives the expected Go package rigidly as `<module>/<api-id>` (= `<module>/proto/<svc>/v1`), so `apx release prepare` (and `--dry-run`) prints a non-fatal `go_package` mismatch warning — `got "<module>/gen/<svc>v1", expected "<module>/proto/<svc>/v1"`. The command exits 0; the warning is expected and harmless. Do not align the `go_package` to silence it (it breaks the ent build), and do not pass `--strict` (that makes the warning fatal). See [Governing the public API locally](../../how-to/model-and-persist/define-a-service/#governing-the-public-api-locally).
{{< /callout >}}
