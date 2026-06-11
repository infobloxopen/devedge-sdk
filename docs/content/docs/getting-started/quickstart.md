---
title: Quickstart
weight: 2
---

Stand up a fail-closed gRPC service in five minutes. By the end you will have a proto with an
authz annotation, generated rules, a running server, and a passing test.

## 1. Prerequisites

Install the SDK and the codegen plugins (see [Installation](../installation/)):

```bash
go get github.com/infobloxopen/devedge-sdk@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-devedge-authz@latest
go install github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-svc@latest
```

## 2. Write a proto with an authz annotation

Each RPC declares its authorization requirement with `(infoblox.authz.v1.rule)`. The verb and
resource are the *only* things a method needs to declare — the framework does the rest.

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

This produces `widget.pb.go`, `widget_grpc.pb.go`, and `widget.authz.go` — the last contains a
generated `WidgetServiceAuthzRules` table you pass straight to the server.

{{< callout type="info" >}}
Prefer no generated file? `authzpb.RulesFromGlobal()` reads the same annotations off the
linked descriptors at runtime. Both produce identical `[]authz.MethodRule`. See
[Annotations](../../concepts/annotations/).
{{< /callout >}}

## 4. Wire the server

`server.New` assembles the full interceptor chain and (optionally) the HTTP gateway. The
`Authorizer` defaults to a **default-deny** dev authorizer, so unless you grant something,
every call is denied — fail-closed by construction.

```go {filename="main.go"}
package main

import (
    "context"
    "log"
    "os/signal"
    "syscall"

    "github.com/example/widget/widgetv1"
    "github.com/infobloxopen/devedge-sdk/authz"
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
RequestID → ErrorMapper → TenantID → grpcauthz (fail-closed) → FieldMask → ETag/412
```

See [server reference](../../reference/server.md) for every `Config` field.

## 5. Test it

A request with no grants must be denied. With this dev authorizer, only `group:admin` is
allowed; everyone else gets `PermissionDenied`:

```go {filename="widget_test.go"}
func TestGetWidget_DeniedForUnknownPrincipal(t *testing.T) {
    // ... dial the server without admin metadata ...
    _, err := client.GetWidget(ctx, &widgetv1.GetWidgetRequest{Id: "w1"})
    if status.Code(err) != codes.PermissionDenied {
        t.Fatalf("expected PermissionDenied, got %v", status.Code(err))
    }
}
```

```bash
go test ./...
```

## What you got for free

- **Fail-closed authz** — an undeclared or ungranted method is denied, no code required.
- **Tenant context** — `account-id` from incoming metadata is on `ctx` for every handler.
- **Clean errors** — internal details are mapped to safe gRPC status codes.
- **ETag/412** — conditional-request preconditions are read and the response ETag is written.

## Next steps

- [Define a service](../../guides/define-a-service/) — the full proto → generate → scaffold loop.
- [Secret fields](../../guides/secret-fields/) — encrypt sensitive fields at rest.
- [API Key Manager tutorial](../../tutorial/api-key-manager/) — build a complete service.
