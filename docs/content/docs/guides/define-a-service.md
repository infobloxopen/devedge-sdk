---
title: Define a Service
weight: 1
---

The full loop: author a proto, run `buf generate`, and get a service scaffold, a repository, and
an authz rule table. The proto is the single source of truth.

## 1. Author the proto

Declare your resource, the RPCs, the HTTP mappings, and the authz rule per method. This is the
[apikey fixture](https://github.com/infobloxopen/devedge-sdk/tree/main/testdata/apikey) shipped
with the SDK:

```proto {filename="apikey.proto"}
syntax = "proto3";
package apikey.v1;

option go_package = "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1;apikeyv1";

import "google/api/annotations.proto";
import "infoblox/authz/v1/authz.proto";

message APIKey {
  string id         = 1;
  string name       = 2;
  string account_id = 3;
  string key_value  = 4 [(infoblox.authz.v1.field).secret = true];
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

## 3. Generate

```bash
buf generate
```

You now have:

| File | From | Contains |
|---|---|---|
| `apikey.pb.go`, `apikey_grpc.pb.go` | base plugins | message types + gRPC stubs |
| `apikey.authz.go` | `protoc-gen-devedge-authz` | `APIKeyServiceAuthzRules []authz.MethodRule` |
| `apikey.svc.go` | `protoc-gen-svc` | the service scaffold, handlers wired to a repository |
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
})
// register the generated service impl, then srv.Serve(ctx)
```

The generated rules feed both the authz interceptor and the field-mask validator. See the
[server reference](../../reference/server.md).

## 5. Choose how secret fields persist

`key_value` is annotated `secret`, so the generated `APIKeyRepository` needs an `Encryptor`. Its
constructor is `NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor)`. See
[Secret fields](../secret-fields/).

## Next

- [Storage shapes](../storage-shapes/) — GORM vs ent.
- [API Key Manager tutorial](../../tutorial/api-key-manager/) — the same proto, end to end.
