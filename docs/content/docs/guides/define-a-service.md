---
title: Define a Service
weight: 1
---

The full loop: author a proto, run `buf generate`, and get a service scaffold, a repository, and
an authz rule table. The proto is the single source of truth.

## 1. Author the proto

Declare your resource, the RPCs, the HTTP mappings, and the authz rule per method. This is the
[apikey fixture](https://github.com/infobloxopen/devedge-sdk/tree/main/testdata/apikey) shipped
with the SDK. For the resource message itself — which field types persist, the framework-managed
fields (`etag`, `delete_time`, `account_id`, …), constraints, and relationships — see
[Model a Resource](../model-a-resource/).

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

## 2. Configure buf

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
The storage shapes (`protoc-gen-storage` for GORM, `protoc-gen-ent` for ent) pull in
`gorm.io/gorm` / `entgo.io/ent`. Generate them into a **module that has those deps** so they
never enter the SDK's own `go.mod`. The SDK's apikey fixture does exactly this — it lives in its
own module under `testdata/apikey/`.
{{< /callout >}}

### Building in your own module (consumer setup)

The `buf.gen.yaml` above is the SDK's **in-repo** setup. In **your own** service module you also
need a `buf.yaml` that resolves the non-local imports — `google/api/annotations.proto` and the two
`infoblox/...` annotation protos — none of which live in your repo by default:

1. **Vendor the annotation protos.** The `infoblox/authz/v1/authz.proto` and
   `infoblox/field/v1/field.proto` schemas are released via `apx` in the canonical
   [`infobloxopen/apis`](https://github.com/infobloxopen/apis) module, but that module ships only
   the **generated Go bindings** (which you pull in step 4) — it does not publish the `.proto`
   source for `buf export`, and there is no public BSR module to export from. The SDK therefore
   ships a byte-identical **mirror** of both files under its module at `proto/infoblox/`; vendor
   from there. Because you copy from the SDK version your `go.mod` already pins, the protos stay in
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

## 3. Generate

```bash
buf generate
```

You now have:

| File | From | Contains |
|---|---|---|
| `apikey.pb.go`, `apikey_grpc.pb.go` | base plugins | message types + gRPC stubs |
| `apikey.authz.go` | `protoc-gen-devedge-authz` | `APIKeyServiceAuthzRules []authz.MethodRule` |
| `apikey.svc.go` | `protoc-gen-svc` | `RegisterAPIKeyService(srv, impl)` — boot-gate + gRPC/gateway registration (you write the handlers) |
| `apikey.storage.go` | `protoc-gen-storage` | `APIKeyModel` + `APIKeyRepository` (GORM) |
| `apikey.pb.gw.go` | gateway plugin | HTTP/JSON gateway registration |
| `ent/schema/api_key.go` | `protoc-gen-ent` | the ent schema (run `go generate ./ent` to build the client) |

## 4. Wire the server

Pass the generated rules to `server.New` and register the generated service:

```go
srv, err := server.New(server.Config{
    GRPCAddr: ":9090",
    HTTPAddr: ":8080",
    Rules:    apikeyv1.APIKeyServiceAuthzRules,
    Authorizer: authz.NewDevAuthorizer(/* grants */),
    // Derive the principal from request metadata in dev so grants can match;
    // swap for a verified-token PrincipalFunc in production. See server reference.
    PrincipalFunc: grpcauthz.DevPrincipalFunc(),
})
// The generated RegisterAPIKeyService runs the boot-time authz gate and registers
// the service on both the gRPC server and the HTTP gateway in one call:
if err := apikeyv1.RegisterAPIKeyService(srv, &apiKeyServer{ /* repo, enc */ }); err != nil {
    log.Fatal(err)
}
// then srv.Serve(ctx)
```

The generated rules feed both the authz interceptor and the field-mask validator. See the
[server reference](../../reference/server.md).

## 5. Choose how secret fields persist

`key_value` is annotated `secret`, so the generated `APIKeyRepository` needs an `Encryptor`. Its
constructor is `NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor)`. See
[Secret fields](../model-a-resource/#secret-fields).

## Next

- [Storage shapes](../storage-shapes/) — GORM vs ent.
- [API Key Manager tutorial](../../tutorial/api-key-manager/) — the same proto, end to end.
