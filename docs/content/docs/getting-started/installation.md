---
title: Installation
weight: 1
---

## Prerequisites

devedge-sdk is a Go module. To build a service with it you need:

| Tool | Version | Why |
|---|---|---|
| **Go** | 1.25+ | the SDK module targets a current Go toolchain |
| **buf** | latest | drives proto compilation and the codegen plugins ([buf.build](https://buf.build)) |
| **protoc-gen-go**, **protoc-gen-go-grpc** | latest | base proto/gRPC code generation |
| **PostgreSQL** | 14+ (prod shapes) | only needed when you use a real GORM/ent backend; the in-memory store and SQLite suffice for tests |
| **HashiCorp Vault** | optional | only for production secret handling via Transit; dev mode uses AES-256-GCM with no external service |

{{< callout type="info" >}}
You do **not** need Vault or Postgres to follow the [Quickstart](../quickstart/) — the dev
encryptor and the in-memory repository run entirely in-process.
{{< /callout >}}

## Add the module

```bash
go get github.com/infobloxopen/devedge-sdk@latest
```

This pulls the runtime packages:

```go
import (
    "github.com/infobloxopen/devedge-sdk/authz"
    "github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
    "github.com/infobloxopen/devedge-sdk/server"
    "github.com/infobloxopen/devedge-sdk/secret"
    "github.com/infobloxopen/devedge-sdk/persistence"
    "github.com/infobloxopen/devedge-sdk/seccheck"
    "github.com/infobloxopen/devedge-sdk/middleware"
)
```

The core packages depend only on the standard library plus gRPC and protobuf. The SDK has
**no ORM dependency** and **no policy-engine dependency** — those live in adapters built *on*
the SDK, or in the generated storage code's own module (so `gorm.io/gorm` never enters the
SDK's `go.mod`).

## Install the codegen plugins

The codegen plugins are `main` packages under the SDK repo. Install them onto your `PATH` so
`buf generate` can invoke them:

```bash
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-svc@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-storage@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-ent@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-devedge-authz@latest
```

| Plugin | Output |
|---|---|
| `protoc-gen-svc` | the service scaffold (`*.svc.go`) |
| `protoc-gen-storage` | a GORM-backed `Repository` (`*.storage.go`) |
| `protoc-gen-ent` | an ent schema (`ent/schema/*.go`) |
| `protoc-gen-devedge-authz` | the `<Service>AuthzRules` `[]MethodRule` table (`*.authz.go`) |

## Verify

```bash
go list -m github.com/infobloxopen/devedge-sdk
which protoc-gen-svc protoc-gen-storage protoc-gen-ent
```

Next: the [Quickstart](../quickstart/).
