---
title: Define a Service
weight: 1
aliases:
  - /docs/guides/define-a-service/
---

This page explains how to define a devedge-sdk service from a Protocol Buffer (proto) definition.
You author a proto, run `buf generate`, and receive a service scaffold, a repository, and an authz
rule table. The proto is the single source of truth for your service's API, storage, and
authorization.

Use this page when you are adding a new resource to an existing service or customizing the scaffold
that `devedge-sdk new service` generates.

{{< callout type="info" >}}
**Starting a new service?** Run
`devedge-sdk new service <name> --resource <Resource> --backend gorm` instead (see the
[Quickstart](../../../getting-started/quickstart/#scaffold-generator)). It generates
a building, authz-gated, persisting project with no hand-edits. This page explains what that
scaffold produces.
{{< /callout >}}

## Author the proto

Declare your resource, the RPCs, the HTTP mappings, and the authz rule per method. The example
below is the [apikey fixture](https://github.com/infobloxopen/devedge-sdk/tree/main/testdata/apikey)
shipped with the SDK. For the resource message itself — which field types persist, the
framework-managed fields (`etag`, `delete_time`, `account_id`, …), constraints, and relationships —
see [Model a Resource](../model-a-resource/).

```proto {filename="apikey.proto"}
syntax = "proto3";
package apikey.v1;

option go_package = "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1;apikeyv1";

import "google/api/annotations.proto";
import "infoblox/authz/v1/authz.proto";
import "infoblox/field/v1/field.proto";

message APIKey {
  string id         = 1;
  string name       = 2;
  string account_id = 3;
  string key_value  = 4 [(infoblox.field.v1.opts) = {secret: true}];
  string key_prefix = 5;
}

service APIKeyService {
  rpc CreateAPIKey(CreateAPIKeyRequest) returns (APIKey) {
    option (google.api.http) = {post: "/v1/apikeys", body: "api_key"};
    option (infoblox.authz.v1.rule) = {verb: "create", resource: "api_keys"};
  }
  rpc GetAPIKey(GetAPIKeyRequest) returns (APIKey) {
    option (google.api.http) = {get: "/v1/apikeys/{id}"};
    option (infoblox.authz.v1.rule) = {verb: "read", resource: "api_keys"};
  }
  rpc ListAPIKeys(ListAPIKeysRequest) returns (ListAPIKeysResponse) {
    option (google.api.http) = {get: "/v1/apikeys"};
    option (infoblox.authz.v1.rule) = {verb: "read", resource: "api_keys"};
  }
  rpc DeleteAPIKey(DeleteAPIKeyRequest) returns (DeleteAPIKeyResponse) {
    option (google.api.http) = {delete: "/v1/apikeys/{id}"};
    option (infoblox.authz.v1.rule) = {verb: "delete", resource: "api_keys"};
  }
}

message CreateAPIKeyRequest  { APIKey api_key = 1; }
message GetAPIKeyRequest     { string id = 1; }
message ListAPIKeysRequest   { int32 page_size = 1; string page_token = 2; }
message ListAPIKeysResponse  { repeated APIKey api_keys = 1; string next_page_token = 2; }
message DeleteAPIKeyRequest  { string id = 1; }
message DeleteAPIKeyResponse {}
```

## Configure buf

A `buf.gen.yaml` lists every plugin in the order they run. The SDK's plugins generate after the
base `protoc-gen-go` / `protoc-gen-go-grpc`:

```yaml {filename="buf.gen.yaml"}
version: v2
inputs:
  - directory: .
plugins:
  - local: protoc-gen-go
    out: .
    opt: paths=source_relative
  - local: protoc-gen-go-grpc
    out: .
    opt: paths=source_relative
  - local: protoc-gen-devedge-authz   # → apikey.authz.go (APIKeyServiceAuthzRules)
    out: .
    opt: paths=source_relative
  - local: protoc-gen-svc             # → apikey.svc.go (service scaffold)
    out: .
    opt: paths=source_relative
  - local: protoc-gen-storage         # → apikey.storage.go (GORM Repository)
    out: .
    opt: paths=source_relative
  - local: protoc-gen-grpc-gateway    # → apikey.pb.gw.go (HTTP/JSON gateway)
    out: .
    opt: paths=source_relative
  - local: protoc-gen-ent             # → ent/schema/api_key.go (ent schema)
    out: .
```

{{< callout type="info" >}}
The storage plugins (`protoc-gen-storage` for GORM, `protoc-gen-ent` for ent) pull in
`gorm.io/gorm` / `entgo.io/ent`. Generate them into a **module that has those deps** so they
never enter the SDK's own `go.mod`. The SDK's apikey fixture does this — it lives in its own
module under `testdata/apikey/`.
{{< /callout >}}

### Consumer module setup

The `buf.gen.yaml` above is the SDK's in-repo setup. In your own service module you also need a
`buf.yaml` that resolves the non-local imports — `google/api/annotations.proto` and the two
`infoblox/...` annotation protos — which do not live in your repo by default.

Follow these steps:

1. **Vendor the annotation protos.** The `infoblox/authz/v1/authz.proto` and
   `infoblox/field/v1/field.proto` schemas are released via `apx` in the canonical
   [`infobloxopen/apis`](https://github.com/infobloxopen/apis) module, but that module ships only
   the **generated Go bindings** (which you pull in step 4) — it does not publish the `.proto`
   source for `buf export`, and there is no public BSR module to export from. The SDK ships a
   byte-identical **mirror** of both files under its module at `proto/infoblox/`; vendor from
   there. Because you copy from the SDK version your `go.mod` already pins, the protos stay in
   lock-step with the bindings you compile against:

   ```bash
   # Pin the SDK, then copy its mirrored annotation protos out of the module cache.
   go get github.com/infobloxopen/devedge-sdk@latest
   SDK=$(go list -m -f '{{.Dir}}' github.com/infobloxopen/devedge-sdk)
   mkdir -p proto/infoblox
   cp -R "$SDK/proto/infoblox/." proto/infoblox/
   ```

   Keep the vendored imports in a directory **separate** from your own protos (buf v2 module roots
   must not overlap):

   ```text
   your-service/
   ├── api/                     # your protos       → one buf module
   │   └── notes.proto
   └── proto/                   # vendored imports   → a second buf module
       └── infoblox/{authz,field}/v1/*.proto
   ```

2. **`buf.yaml`** — declare both roots plus the googleapis dep, then run `buf dep update` (this
   writes `buf.lock`; it is required before the first `buf generate`):

   ```yaml {filename="buf.yaml"}
   version: v2
   modules:
     - path: api
     - path: proto
   deps:
     - buf.build/googleapis/googleapis   # provides google/api/*.proto
   ```
   ```bash
   buf dep update
   ```

3. **`buf.gen.yaml`** — generate only your module (`inputs: api`) and use `module=` output so the
   generated Go lands at your import path:

   ```yaml {filename="buf.gen.yaml"}
   version: v2
   inputs:
     - directory: api
   plugins:
     - {local: protoc-gen-go,            out: ., opt: module=github.com/example/notes}
     - {local: protoc-gen-go-grpc,       out: ., opt: module=github.com/example/notes}
     - {local: protoc-gen-devedge-authz, out: ., opt: module=github.com/example/notes}
     - {local: protoc-gen-svc,           out: ., opt: module=github.com/example/notes}
     - {local: protoc-gen-grpc-gateway,  out: ., opt: module=github.com/example/notes}
   ```

4. **`go.mod`** — the generated code imports the canonical annotation bindings:

   ```bash
   go get github.com/infobloxopen/apis/proto/infoblox/authz@latest
   go get github.com/infobloxopen/apis/proto/infoblox/field@latest
   ```

## Generate

```bash
buf generate
```

You now have:

| File | From | Contains |
|---|---|---|
| `apikey.pb.go`, `apikey_grpc.pb.go` | base plugins | message types + gRPC stubs |
| `apikey.authz.go` | `protoc-gen-devedge-authz` | `APIKeyServiceAuthzRules []authz.MethodRule` |
| `apikey.svc.go` | `protoc-gen-svc` | the generated default CRUD handler `APIKeyServiceCRUDHandler` + `NewAPIKeyServiceHandler` / `RegisterAPIKeyServiceWithRepository` / `RegisterAPIKeyService` (no hand-written handler for pure CRUD) |
| `apikey.storage.go` | `protoc-gen-storage` | `APIKeyModel` + `APIKeyRepository` (GORM) |
| `apikey.pb.gw.go` | gateway plugin | HTTP/JSON gateway registration |
| `ent/schema/api_key.go` | `protoc-gen-ent` | the ent schema (run `go generate ./ent` to build the client) |

## Wire the server

A scaffolded service runs on the `servicekit` host. `protoc-gen-svc` emits an importable
`<Service>Module`, and the scaffold's `cmd/<svc>/main.go` hands it to `servicekit.Run`, which builds
the one shared server and serves under the fail-closed boot gate. For the runtime these pieces form,
see the [servicekit reference](../../../reference/servicekit/).

For a pure-CRUD service there is no hand-written handler and no hand-listed rules. The generated
module wires the generated CRUD handler over your repository, and `Run` hosts it:

```go
// Build the generated repository, wrap it in the generated module, and host it.
repo := apikeyv1.NewAPIKeyRepository(db, enc) // generated GORM repo (enc for the secret field)
err := servicekit.Run(servicekit.HostConfig{
    Modules:  []servicekit.Module{apikeyv1.APIKeyServiceModule(apikeyv1.APIKeyServiceModuleOptions{Repo: repo})},
    GRPCAddr: ":9090",
    HTTPAddr: ":8080",
    // No Rules to pass: the generated module carries APIKeyServiceAuthzRules and
    // contributes them when it registers.
    Authorizer: authz.NewDevAuthorizer(/* grants */),
    // Derive the principal from request metadata in dev so grants can match;
    // swap for a verified-token PrincipalFunc in production. See server reference.
    PrincipalFunc: grpcauthz.DevPrincipalFunc(),
    Context:       ctx,
})
```

The generated module's `Register` calls `RegisterAPIKeyServiceWithRepository` on the shared server,
which constructs the generated CRUD handler, registers it on gRPC and the HTTP gateway, and
contributes the service's authz rules. The boot-time completeness gate runs when `Run` calls
`server.Serve`.

**Custom or non-CRUD logic?** Replace the generated module with a hand-written `servicekit.Module`:
embed the generated `APIKeyServiceCRUDHandler`, override the methods you change (and implement any
custom RPCs the generator left `Unimplemented`), then register your handler with
`RegisterAPIKeyService(app.Server, h)`. The
[Add a custom method or second resource](../custom-methods/) how-to walks the full recipe; see
[codegen → protoc-gen-svc](../../../reference/codegen/#protoc-gen-svc) for the override mechanics.

The generated rules feed both the authz interceptor and the field-mask validator. See the
[servicekit reference](../../../reference/servicekit/) and the
[server reference](../../../reference/server/).

## Error handling

The `ErrorMapperUnary` interceptor, installed automatically by `server.New`, converts persistence
sentinels to gRPC status codes so your handlers do not need to map them manually:

| Sentinel | gRPC code |
|---|---|
| `persistence.ErrNotFound` | `codes.NotFound` |
| `persistence.ErrConflict` | `codes.AlreadyExists` |
| `persistence.ErrPreconditionFailed` | `codes.FailedPrecondition` |
| `persistence.ConstraintError(err)` (unique/FK violation) | `codes.AlreadyExists` or `codes.FailedPrecondition` |
| `*persistence.FieldViolationError` | `codes.InvalidArgument` (with field detail) |

Return the error from the repository call directly. The interceptor handles the translation.
Hand-calling `status.Error(codes.NotFound, …)` is redundant and risks diverging from the
framework's AIP-193 error detail format.

```go
func (s *apiKeyServer) GetAPIKey(ctx context.Context, req *apikeyv1.GetAPIKeyRequest) (*apikeyv1.APIKey, error) {
    k, err := s.repo.Get(ctx, req.Id)
    if err != nil {
        return nil, err // ErrNotFound → codes.NotFound; other sentinels mapped automatically
    }
    return k, nil
}
```

See [middleware → Error mapper](../../../reference/middleware/#error-mapper) and
[persistence → Errors](../../../reference/persistence/#errors).

## Secret fields

`key_value` is annotated `secret`, so the generated `APIKeyRepository` requires an `Encryptor`. Its
constructor is `NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor)`. See
[Secret fields](../../../how-to/secure/secret-fields/).

## Governing the public API locally

The proto is the public, apx-governed contract. Run the same gates locally that CI runs before you
push. A service scaffolded with `devedge-sdk new service` ships Makefile targets for each:

```sh
make api-lint       # STANDARD lint of the public proto (apx lint)
make api-breaking   # backward-compatibility check vs the last committed proto (apx breaking --against HEAD)
make api-release    # prepare a versioned release (apx release prepare … --lifecycle experimental)
```

Or call `apx` directly: `apx lint`, `apx breaking --against HEAD`, `apx release prepare proto/<svc>/v1
--version v1.0.0-alpha.1 --lifecycle experimental --dry-run`.

- **`api-breaking`** catches an accidental breaking change before it lands. It requires an initialized
  git repo with at least one commit — run `git init && git add . && git commit -m "initial"` first if
  this is a fresh scaffold. After that, with no prior API tag, it passes. Once released, compare
  against the released tag instead.
- **`api-release`** on the **ent** scaffold prints a **non-fatal `go_package` mismatch** warning — `got
  "<module>/gen/<svc>v1", expected "<module>/proto/<svc>/v1"`. This is expected and harmless: the
  generated Go must be a single directory segment under `gen/` so the sibling generated `ent/` package
  compiles, which is not the `<module>/<api-id>` layout apx derives by default. The command exits 0 —
  do not realign the `go_package` (it breaks the ent build) and do not pass `--strict` (that turns the
  warning fatal). See [codegen → buf.gen.yaml](../../../reference/codegen/#configuring-bufgenyaml).

## Next steps

- [Storage shapes](../storage-shapes/) — GORM vs ent.
- [API Key Manager tutorial](../../../tutorial/api-key-manager/) — the same proto, end to end.
