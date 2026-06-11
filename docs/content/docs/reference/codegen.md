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

Generates the service scaffold: handler stubs wired to a `persistence.Repository`, request
validation, and the CRUDL plumbing that maps API verbs onto repository methods.

## protoc-gen-storage

Generates a GORM-backed `Repository` for each message. For a message named `APIKey` it emits:

- **`APIKeyModel`** — the GORM model. Standard columns plus `ETag`, `CreatedAt`, `UpdatedAt`, and
  a soft-delete `DeletedAt`.
- **`toModel_APIKey` / `fromModel_APIKey`** — converters. They **skip** repeated fields (TODO:
  JSONB), nested messages (TODO: serialization), and secret fields.
- **`APIKeyRepository`** + **`NewAPIKeyRepository`** — `Get`, `List`, `Create`, `Update`,
  `Delete`, satisfying `persistence.Repository[*APIKey, string]` (a compile-time `var _` check is
  emitted).

### Secret fields

A field marked `(infoblox.authz.v1.field).secret = true` does **not** get a plaintext column.
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
