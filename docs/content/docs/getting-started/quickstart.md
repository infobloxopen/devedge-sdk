---
title: Quickstart
weight: 2
---

This quickstart walks you through creating a fail-closed gRPC service with the devedge-sdk in
about five minutes. By the end you have a proto with an authz annotation, generated rules, a
running server, and a passing test. Use this page as the starting point before moving on to the
full [API Key Manager tutorial](../../tutorial/api-key-manager/).

## Scaffold generator

The scaffold generator is the fastest path to a running service. One command produces a project
governed by apx, gated by authz, and backed by a persistence layer. The generated project builds
and passes its smoke test without any manual file edits.

```bash
go install github.com/infobloxopen/devedge-sdk/cmd/devedge-sdk@latest

devedge-sdk new service orders --resource Order --backend gorm
cd orders
make test            # boots the server + one tenant-scoped CRUD round-trip — green
```

The generated project includes:

- `proto/orders/v1/orders.proto` — a CRUD `OrderService` with an `(infoblox.authz.v1.rule)` on every RPC.
- `apx.yaml` and `.github/workflows/apx-release.yml` — the apx app-repo files.
- `buf.yaml` and `buf.gen.yaml` — pre-wired with the six SDK codegen plugins.
- Vendored `infoblox/{authz,field,storage}` annotation mirrors.
- `server/main.go` — calls `server.New(...)` with the generated `OrderServiceAuthzRules`.
- A generated GORM model and repository (git-ignored; engine deps in your `go.mod` only).
- A passing smoke test.

The generator requires `apx` and `buf` on `PATH` and calls `apx init app` so the app layout
stays current with apx. Available flags:

| Flag | Description |
|---|---|
| `--backend gorm\|ent` | Persistence backend. Both produce a building, persisting service with no hand-written persistence wiring. The `ent` backend runs the `buf`→`entc` two-step for you. |
| `--module` | Go module path. Defaults to `github.com/<org>/<name>`. |
| `--org` | GitHub org. Defaults to `infobloxopen`. |
| `--no-generate` | Skip the first `buf generate`. |
| `--force` | Scaffold into a non-empty directory. |

{{< callout type="info" >}}
**Set `--module` if your module path is not under `github.com/infobloxopen`.** The command above
generates a module rooted at `github.com/infobloxopen/orders`. Pass your own path so the
generated `go.mod` and imports are correct:

```bash
devedge-sdk new service orders --resource Order --backend gorm \
  --module github.com/you/orders
```
{{< /callout >}}

The rest of this page walks the manual flow so you understand what the scaffold generates. The
manual sequence involves roughly ten hand-authored artifacts: a two-module `buf.yaml`, a
seven-plugin `buf.gen.yaml` (where one plugin takes no `module=` and another an optional
`dialect=`), byte-identical annotation mirrors, and the `server.New(...)` wiring. The scaffold
produces all of these from a single command.

## 1. Prerequisites

Install the SDK and every plugin the `buf.gen.yaml` in step 3 invokes — the two SDK plugins plus
the base proto/gRPC plugins (see [Installation](../installation/) for the full table):

```bash
go get github.com/infobloxopen/devedge-sdk@latest
# SDK plugins:
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-devedge-authz@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-svc@latest
# base proto/gRPC plugins the buf.gen.yaml below also runs (independently versioned):
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

If you also wire a backend (`protoc-gen-storage` for GORM or `protoc-gen-ent` for ent) or the
HTTP/JSON gateway (`protoc-gen-grpc-gateway`), install those too — see
[Installation](../installation/#codegen-plugins).

## 2. Write a proto with an authz annotation

Each RPC declares its authorization requirement with `(infoblox.authz.v1.rule)`. The verb and
resource are the only things a method needs to declare — the framework handles the rest.

```proto {filename="widget.proto"}
syntax = "proto3";
package widget.v1;

option go_package = "github.com/example/widget/widgetv1;widgetv1";

import "infoblox/authz/v1/authz.proto";

service WidgetService {
  rpc GetWidget(GetWidgetRequest) returns (Widget) {
    option (infoblox.authz.v1.rule) = {verb: "get", resource: "widget:{id}"};
  }
  rpc CreateWidget(CreateWidgetRequest) returns (Widget) {
    option (infoblox.authz.v1.rule) = {verb: "create", resource: "widget"};
  }
}

message Widget            { string id = 1; string name = 2; }
message GetWidgetRequest  { string id = 1; }
message CreateWidgetRequest { Widget widget = 1; }
```

## 3. Generate

A minimal `buf.gen.yaml`:

```yaml {filename="buf.gen.yaml"}
version: v2
plugins:
  - local: protoc-gen-go
    out: .
    opt: paths=source_relative
  - local: protoc-gen-go-grpc
    out: .
    opt: paths=source_relative
  - local: protoc-gen-devedge-authz   # emits WidgetServiceAuthzRules ([]authz.MethodRule)
    out: .
    opt: paths=source_relative
```

```bash
buf generate
```

This produces `widget.pb.go`, `widget_grpc.pb.go`, and `widget.authz.go`. The last file contains
a generated `WidgetServiceAuthzRules` table you pass directly to the server.

{{< callout type="info" >}}
Prefer no generated file? `authzpb.RulesFromGlobal()` reads the same annotations off the
linked descriptors at runtime. Both produce identical `[]authz.MethodRule`. See
[Annotations](../../concepts/annotations/).
{{< /callout >}}

## 4. Wire the server

`server.New` assembles the full interceptor chain and, optionally, the HTTP gateway. The
`Authorizer` defaults to a default-deny dev authorizer, so every call is denied unless you grant
it explicitly.

```go {filename="main.go"}
package main

import (
    "context"
    "log"
    "os/signal"
    "syscall"

    "github.com/example/widget/widgetv1"
    "github.com/infobloxopen/devedge-sdk/authz"
    "github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
    "github.com/infobloxopen/devedge-sdk/server"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    srv, err := server.New(server.Config{
        GRPCAddr: ":9090",
        HTTPAddr: ":8080", // optional HTTP/JSON gateway; omit to run gRPC-only
        Rules:    widgetv1.WidgetServiceAuthzRules, // generated in step 3
        // Dev decision point — grant group:admin everything. Swap for an
        // OPA/Cedar/remote Authorizer in production; nothing else changes.
        Authorizer: authz.NewDevAuthorizer(authz.Grant{
            Tenant:   "t1",
            Subjects: []string{"group:admin"},
            Verbs:    []authz.Verb{"*"},
            Resource: "*",
        }),
        // Derive the principal from request metadata so the grant above can match
        // (account-id → Tenant, groups → group:<name>). Without this the principal
        // is empty and every call is denied. Use a verified-token func in prod.
        PrincipalFunc: grpcauthz.DevPrincipalFunc(),
    })
    if err != nil {
        log.Fatal(err)
    }

    // Register your service implementation on the underlying *grpc.Server.
    widgetv1.RegisterWidgetServiceServer(srv.GRPCServer(), &widgetServer{})

    log.Printf("serving gRPC on %s", srv.GRPCAddr())
    if err := srv.Serve(ctx); err != nil { // blocks until ctx is cancelled
        log.Fatal(err)
    }
}
```

The chain `server.New` builds, outermost first:

```
RequestID → ErrorMapper → TenantID → grpcauthz (fail-closed) → FieldMask → ETag/412 → ReadMask → ValidateOnly → Deduplicate
```

See [server reference](../../reference/server.md) for every `Config` field.

## 5. Test it

A request with no grants must be denied. With this dev authorizer, only `group:admin` is
allowed; everyone else receives `PermissionDenied`:

```go {filename="widget_test.go"}
func TestGetWidget_DeniedForUnknownPrincipal(t *testing.T) {
    // ... dial the server without admin metadata ...
    _, err := client.GetWidget(ctx, &widgetv1.GetWidgetRequest{Id: "w1"})
    if status.Code(err) != codes.PermissionDenied {
        t.Fatalf("expected PermissionDenied, got %v", status.Code(err))
    }
}
```

A caller that presents the granted identity is allowed. `DevPrincipalFunc` reads the identity from
metadata (`account-id` → tenant, `groups` → `group:<name>`):

```go
md := metadata.Pairs("account-id", "t1", "groups", "admin")
ctx = metadata.NewOutgoingContext(ctx, md)
_, err = client.GetWidget(ctx, &widgetv1.GetWidgetRequest{Id: "w1"})
// no longer PermissionDenied — the group:admin grant matches.
```

```bash
go test ./...
```

## What the server provides

- **Fail-closed authz** — an undeclared or ungranted method is denied, no code required.
- **Tenant context** — `account-id` from incoming metadata is on `ctx` for every handler.
- **Clean errors** — internal details are mapped to safe gRPC status codes.
- **ETag/412** — conditional-request preconditions are read and the response ETag is written.

## Error handling

The `ErrorMapperUnary` interceptor (part of the chain `server.New` builds) automatically converts
the persistence error sentinels to canonical gRPC status codes:

| Sentinel | gRPC code |
|---|---|
| `persistence.ErrNotFound` | `codes.NotFound` |
| `persistence.ErrConflict` | `codes.AlreadyExists` |
| `persistence.ErrPreconditionFailed` | `codes.FailedPrecondition` |
| `persistence.ConstraintError(err)` (unique/FK violation) | `codes.AlreadyExists` or `codes.FailedPrecondition` |
| `*persistence.FieldViolationError` | `codes.InvalidArgument` (with field detail) |

Return the sentinel from your handler rather than mapping it yourself. If your handler (or the
repository it calls) returns `persistence.ErrNotFound`, the interceptor converts it to a
`NotFound` status before the client sees it — you do not need to call
`status.Error(codes.NotFound, …)` yourself. Hand-mapping duplicates the logic, creates
inconsistencies, and leaks internal detail.

```go
func (s *server) GetWidget(ctx context.Context, req *pb.GetWidgetRequest) (*pb.Widget, error) {
    w, err := s.repo.Get(ctx, req.Id)
    if err != nil {
        return nil, err  // propagate — ErrorMapperUnary maps ErrNotFound → NotFound
    }
    return w, nil
}
```

See [middleware → Error mapper](../../reference/middleware/#error-mapper) and
[persistence → Errors](../../reference/persistence/#errors) for the full mapping and defense-in-depth
details.

## Next steps

- [Define a service](../../how-to/model-and-persist/define-a-service/) — the full proto → generate → scaffold loop.
- [Secret fields](../../how-to/model-and-persist/model-a-resource/#secret-fields) — encrypt sensitive fields at rest.
- [API Key Manager tutorial](../../tutorial/api-key-manager/) — build a complete service.
