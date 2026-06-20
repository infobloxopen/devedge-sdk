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
| **apx** | 0.12.1+ | declares/governs the public API surface; the `devedge-sdk new` scaffold shells out to `apx init app` ([infobloxopen/apx](https://github.com/infobloxopen/apx)) |
| **protoc-gen-go**, **protoc-gen-go-grpc** | latest | base proto/gRPC code generation |
| **PostgreSQL** | 14+ (prod shapes) | only needed when you use a real GORM/ent backend; the in-memory store and SQLite suffice for tests |
| **HashiCorp Vault** | optional | only for production secret handling via Transit; dev mode uses AES-256-GCM with no external service |

{{< callout type="info" >}}
You do **not** need Vault or Postgres to follow the [Quickstart](../quickstart/) — the dev
encryptor and the in-memory repository run entirely in-process.
{{< /callout >}}

Check your apx version with `apx --version` (there is no `apx version` subcommand):

```bash
apx --version   # e.g. "apx 0.12.1 (...)" — need 0.12.1+
```

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

## Install the scaffold CLI (recommended)

The fastest way to start is the `devedge-sdk` CLI. It scaffolds a complete, building, authz-gated,
persisted service in one command — declaring the **public API surface** as an [`apx`](https://github.com/infobloxopen/apx)
`app` module and generating the **internal models** locally with the SDK plugins (which it installs and
invokes for you):

```bash
go install github.com/infobloxopen/devedge-sdk/cmd/devedge-sdk@latest

devedge-sdk new service notes --resource Note --backend gorm   # or --backend ent
```

This emits the `buf.yaml`/`buf.gen.yaml`/`apx.yaml` wiring, an annotated example proto, the server, and a
smoke test, then runs the first `buf generate`. See the [Quickstart](../quickstart/) for the full walk-through.
The manual plugin install below is what the CLI does under the hood — useful when wiring an existing repo
by hand (see [Define a service](../../guides/define-a-service/)).

## Install the codegen plugins

The codegen plugins are `main` packages under the SDK repo. Install them onto your `PATH` so
`buf generate` can invoke them:

```bash
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-svc@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-storage@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-ent@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-devedge-authz@latest
```

If you expose an HTTP/JSON gateway (the quickstart's `curl` examples and the
[Define a service](../../guides/define-a-service/) buf template both do), also install the
**third-party** grpc-gateway plugin. It is independently versioned, so `@latest` is correct here:

```bash
go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
```

Its `google/api/*.proto` imports (`annotations.proto`, `http.proto`) come from the
`buf.build/googleapis/googleapis` module — add it to your `buf.yaml` `deps` and run `buf dep update`
(see [Define a service](../../guides/define-a-service/)).

| Plugin | Output |
|---|---|
| `protoc-gen-svc` | the service scaffold (`*.svc.go`) |
| `protoc-gen-storage` | a GORM-backed `Repository` (`*.storage.go`) |
| `protoc-gen-ent` | an ent schema (`ent/schema/*.go`) |
| `protoc-gen-devedge-authz` | the `<Service>AuthzRules` `[]MethodRule` table (`*.authz.go`) |

## Verify

`go list -m` only works **inside a module** — run it from a directory with a `go.mod` (or create one
first with `go mod init`), otherwise it fails with `go: cannot find main module`:

```bash
# In a module (or: `go mod init example.com/scratch` in an empty dir first):
go list -m github.com/infobloxopen/devedge-sdk

# The plugins are plain executables on PATH — `which` works anywhere:
which protoc-gen-svc protoc-gen-storage protoc-gen-ent protoc-gen-devedge-authz
```

Next: the [Quickstart](../quickstart/).
