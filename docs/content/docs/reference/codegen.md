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
| `protoc-gen-ent` | messages | an ent schema (`ent/schema/*.go`) |

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
  `gorm.DeletedAt`. (The `APIKey` example opts in, so its model has `DeletedAt`.)
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

Generates an ent schema for each message. Run `go generate ./ent` to produce the type-safe client
from the schema. The ent shape enforces tenant scoping and secret-field handling through ent's
**privacy layer** and **hooks** (applied by a generated mixin), so the invariants hold even for
ad-hoc graph traversals — not just CRUD. The SDK wiring exposes a constructor that satisfies the
neutral seam:

```go
func NewAPIKeyEntRepository(client *ent.Client, enc secret.Encryptor) persistence.Repository[*APIKey, string]
```

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
