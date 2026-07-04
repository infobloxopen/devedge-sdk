---
title: servicekit
weight: 8
---

```go
import "github.com/infobloxopen/devedge-sdk/servicekit"
```

Package `servicekit` is the runtime a scaffolded service runs on. It splits a service into an
importable [`Module`](#module) (the service's domain wiring) and an executable host
([`HostConfig`](#hostconfig) + [`Run`](#run)) that builds one shared `server.Server`, registers the
modules on it, and serves under the fail-closed boot gate. `devedge-sdk new service` generates a
host in `cmd/<svc>/main.go` and a module in `module/`, both built on this package.

Use this reference when you change how a scaffolded service is hosted: adding a custom gRPC handler,
hosting a second resource, registering a readiness check, or composing several modules into one
binary. For the task recipe, see
[Add a custom method or second resource](../../how-to/model-and-persist/custom-methods/).

The package lives in the SDK's root module and depends only on the SDK's root interfaces
(`server.Server`, `persistence.Repository`, `events.Bus`, `health.Check`, `authz.MethodRule`, and
`log/slog`). Concrete backends (gorm, ent, otel, kafka) stay in the optional adapter sub-modules, so
the core stays dependency-light.

## Module and host

A service is two parts:

- A **module** owns domain behavior: its resources, handlers, repositories, migrations, events,
  config schema, health checks, and authz rules. A module is importable and self-describing.
- A **host** owns process behavior: listen addresses, flags, environment, the database connection,
  the logger, signal handling, and process exit. The host hands its modules to [`Run`](#run).

The same module runs two ways with no code change. A standalone host gives `Run` one module; a
composed "suite" host gives `Run` several. See [Composable services](#composable-services).

{{< callout type="info" >}}
**A module never owns process lifetime.** It does not parse flags, open the process-wide database,
start listeners, install a signal handler, or call `os.Exit`. Those belong to the host. A module's
`Register` only wires its domain onto the shared host it is handed.
{{< /callout >}}

## Module

```go
type Module interface {
    Descriptor() Descriptor
    Register(ctx context.Context, app *App) error
}
```

`Module` is the unit of composition. A service that runs `protoc-gen-svc` gets a generated
`<Service>Module(<Service>ModuleOptions{...})` constructor that returns a `Module`. The generated
module's `Descriptor` is populated from proto facts, and its `Register` wires the generated CRUD
handler onto the shared server.

| Method | Purpose |
|---|---|
| `Descriptor() Descriptor` | Returns the module's static facts. The host calls it before `Register` to validate the composition, so it must be safe to call early and must return the same value every call. |
| `Register(ctx, app)` | Wires the module onto the shared host. It registers the gRPC service, the HTTP gateway, and the authz rules on `app.Server` (typically through the generated `Register<Service>` helper), then registers health checks, event handlers, and background jobs through the [`App`](#app) it is handed. |

A `Register` implementation must not start listeners, parse flags, call `os.Exit`, or otherwise take
over the process.

## Descriptor

```go
type Descriptor struct {
    ID          string
    DisplayName string
    Version     string

    Methods    []string
    AuthzRules []authz.MethodRule
    Routes     []RouteDescriptor
    Resources  []ResourceDescriptor

    Config         ConfigDescriptor
    Database       DatabaseDescriptor
    Events         EventDescriptor
    HealthChecks   []HealthDescriptor
    BackgroundJobs []JobDescriptor

    Requires      Compatibility
    FailurePolicy FailurePolicy
}
```

`Descriptor` is a module's static, introspectable facts — known before boot so the host can validate
the composition and so tooling can describe a suite without running it. The generated
`<Service>Module` populates the proto-derived fields (`ID`, `Methods`, `AuthzRules`, `Resources`);
hand-written extras flow in through the module's options.

| Field | Required | Notes |
|---|---|---|
| `ID` | **yes** | Stable, unique module identifier (e.g. `"orders"`). The host keys uniqueness, the config prefix default, and the database namespace on it. |
| `DisplayName` | no | Human-friendly label (e.g. `"Orders Service"`). |
| `Version` | no | The module's semantic version (e.g. `"v0.4.1"`). |
| `Methods` | yes for served methods | The gRPC full methods the module registers (e.g. `"/orders.v1.OrderService/CreateOrder"`). The host uses them to detect duplicate service names across modules. Use the generated `<Service>_<Method>_FullMethodName` constants. |
| `AuthzRules` | yes when methods are served | The module's declared authz rules — the generated `<Service>AuthzRules`. Carried here so the host can detect duplicate permission names before boot. |
| `Routes` | no | The HTTP gateway route prefixes or hostnames the module serves, for duplicate-prefix detection. |
| `Resources` | no | The API resources the module owns, module-qualified (e.g. `"orders.order"`), for catalog and duplicate detection. |
| `Config` | no | The module's typed config schema and prefix. See [`ConfigDescriptor`](#config-events-and-database-descriptors). |
| `Database` | no | The module's namespace policy and migrations filesystem. See [`DatabaseDescriptor`](#config-events-and-database-descriptors). |
| `Events` | no | The event types the module publishes and subscribes to, plus its outbox. |
| `HealthChecks` | no | The readiness checks the module contributes. |
| `BackgroundJobs` | no | The supervised background jobs the module runs. |
| `Requires` | no | The SDK, Go, and Postgres version ranges the module needs, for composition-time gating. |
| `FailurePolicy` | no | The module's failure posture in a composed host. See [Failure policy](#failure-policy). |

### Config, events, and database descriptors

These nested descriptors declare the seams the host wires per module:

```go
type ConfigDescriptor struct {
    Prefix   string         // per-module config namespace (e.g. "orders")
    Schema   any            // pointer to the module's typed config struct
    Defaults map[string]any // programmatic defaults beneath the struct default: tags
}

type DatabaseDescriptor struct {
    Isolation   IsolationPolicy // namespacing policy; empty defers to the host default
    Schema      string          // preferred Postgres schema (defaults to the module ID)
    TablePrefix string          // table prefix for prefix-isolation engines
    Migrations  MigrationsFS    // the module's embedded migrations (satisfied by embed.FS)
}

type EventDescriptor struct {
    Publishes  []EventType     // globally-unique event type names the module emits
    Subscribes []EventType     // event type names the module consumes
    Outbox     OutboxDescriptor
}
```

`MigrationsFS` is an alias for the standard `io/fs.FS`, so an `embed.FS` satisfies it directly. The
host reads `Migrations` and runs them per module under an advisory lock; a module never runs its own
migrations from `init`. See
[Add a custom method or second resource](../../how-to/model-and-persist/custom-methods/).

## App

```go
type App struct {
    Server  *server.Server
    Config  ConfigProvider
    DB      DatabaseRegistry
    Events  EventRegistry
    Health  HealthRegistry
    Logger  *slog.Logger
    Metrics MetricsRegistry
}
```

`App` is the running host's shared services, handed to each [`Module.Register`](#module). Each module
receives its own `App`: its registry seams are scoped to that module's ID, so config, database
namespace, metrics, and the event and job helpers all attribute their work to the registering module.

| Field | What a module does with it |
|---|---|
| `Server` | The one shared `server.Server` the module registers its gRPC service, gateway, and authz rules on — typically by calling the generated `Register<Service>(app.Server, impl)` or `Register<Service>WithRepository(app.Server, repo)`. The server's boot-time completeness gate validates the combined surface of every module at `Serve`. |
| `Config` | Loads the module's typed config from the host's sources, scoped to the module's prefix. Call `app.Config.Load(&cfg)` instead of reading global environment. |
| `DB` | Resolves the module's namespaced database identity. A single-module host yields a zero-qualification namespace (bare tables, unchanged). |
| `Events` | Declares the module's outbox so the host owns its relay and consumer lifecycle. |
| `Health` | Registers the module's readiness checks on the shared server. Call `app.Health.Register("<id>.<check>", check)`. |
| `Logger` | The host logger, scoped to the module's ID. |
| `Metrics` | A per-module metric namespace over the SDK's OTel metrics. |

`App` also carries per-module helpers the host uses to own dispatchers and jobs across a composition:

```go
func (a *App) ModuleID() string
func (a *App) Bus() events.Bus
func (a *App) RegisterOutboxRelay(cfg OutboxRelayConfig) error
func (a *App) Subscribe(cfg ConsumerConfig, handlers ...EventHandler) error
func (a *App) RegisterBackgroundJob(name string, fn func(ctx context.Context) error) error
```

| Method | Purpose |
|---|---|
| `ModuleID` | The ID of the module this `App` is scoped to. |
| `Bus` | The host's shared event bus. A module rarely needs it directly — publishing flows through the outbox and subscribing through `Subscribe` — but a module bridging to an external transport can reach it here. |
| `RegisterOutboxRelay` | Declares the module's transactional outbox so the host starts exactly one relay for it, even in a composed binary. Call once from `Register`. |
| `Subscribe` | Registers the module's event handlers so the host starts exactly one consumer for the module over the shared bus. Subscribing to an event type no module publishes is rejected at boot. |
| `RegisterBackgroundJob` | Registers a supervised job the host runs in the module's bulkhead until the context is cancelled. |

{{< callout type="info" >}}
**Same binary does not mean direct calls.** Even when two modules share one process and one
database, a cross-module reaction goes through the durable outbox, the relay, the shared bus, and the
consumer — never a direct handler import. See [Events](../../concepts/events/).
{{< /callout >}}

### App.Server bridge

`app.Server` is the bridge between a module and the generated `Register<Service>` helpers. The
generated code registers a gRPC service on a `*server.Server`, so a module's `Register` passes
`app.Server` to it:

```go
func (m *ordersModule) Register(_ context.Context, app *servicekit.App) error {
    return orderv1.RegisterOrderServiceWithRepository(app.Server, m.repo)
}
```

For a custom handler, register the handler instead of the repository:

```go
func (m *ordersModule) Register(_ context.Context, app *servicekit.App) error {
    return orderv1.RegisterOrderService(app.Server, m.handler)
}
```

Either call records the service's methods, contributes its `<Service>AuthzRules`, registers the
implementation on gRPC, and wires the HTTP gateway. The boot-time authz completeness gate runs later,
at `server.Serve`. See [codegen → protoc-gen-svc](../codegen/#protoc-gen-svc) for the generated
helpers and [server → Server methods](../server/#server-methods) for the `Serve` gate.

## HostConfig

```go
type HostConfig struct {
    Modules []Module

    GRPCAddr string
    HTTPAddr string

    HTTPHandlers []server.HTTPHandler

    Authorizer    authz.Authorizer
    PrincipalFunc grpcauthz.PrincipalFunc
    Authenticator authn.Authenticator
    Logger        *slog.Logger

    ConfigSources []config.Source
    Context       context.Context

    Database *DatabaseConfig
    Migrate  MigrationRunner
    Bus      events.Bus

    FailurePolicies      map[string]FailurePolicy
    DefaultFailurePolicy FailurePolicy
}
```

`HostConfig` is the process-level configuration the host owns. The same shape drives a standalone
binary (one module) and a composed suite (several modules).

| Field | Required | Default | Notes |
|---|---|---|---|
| `Modules` | **yes** | — | The modules to compose into this host. |
| `GRPCAddr` | no | `server.DefaultGRPCAddr` (`":9090"`) | The shared gRPC listen address. |
| `HTTPAddr` | no | `""` (disabled) | The shared HTTP gateway address. |
| `HTTPHandlers` | no | `nil` | `net/http` handlers mounted on the one shared HTTP server (OIDC provider endpoints, webhooks, a login UI, static assets) alongside the module gateways. Probes always win; a `/` handler replaces the gateway catch-all; requires `HTTPAddr`. See [`server.HTTPHandler`](../server/#httphandler). |
| `Authorizer` | no | default-deny dev authorizer | The shared decision point handed to the one server. |
| `PrincipalFunc` | no | `nil` → empty principal | Derives the principal from each request. Without it every non-public method is denied. Use `grpcauthz.DevPrincipalFunc()` in dev, a verified-token function in production. |
| `Authenticator` | no | `nil` (no verify stage) | Inserts the authentication interceptor before authz on the one shared server: it verifies the request bearer (signature + `iss`/`aud`/`exp`) and stashes the verified `authz.Principal`, which the authorizer reads via `authn.VerifiedPrincipal` (`PrincipalFunc` defaults to it). An invalid bearer → `codes.Unauthenticated`. See [`server.Config.Authenticator`](../server/#config) and [Add authentication](../../how-to/secure/add-authentication/). |
| `Logger` | no | `slog.Default()` | The host logger; each module gets a child scoped to its ID. |
| `ConfigSources` | no | `nil` | The configuration sources the host loads module config from, layered per module prefix. |
| `Context` | no | `nil` → cancelled on SIGTERM/Interrupt | The host's root context; `Run` serves until it is cancelled. |
| `Database` | no | `nil` (no shared DB) | The shared database a composed host's modules namespace themselves within. See [`DatabaseConfig`](#databaseconfig). |
| `Migrate` | no | `nil` (no migration) | The host's per-module migration runner. `Run` calls it once per module, after the module's namespace is allocated and before the module registers. servicekit is ORM-free, so the host supplies a runner backed by the gormtx or entrepo adapter. |
| `Bus` | no | in-process membus | The shared event bus the host owns one relay and one consumer per module over. |
| `FailurePolicies` | no | `nil` | Overrides a module's declared `FailurePolicy` per module ID. |
| `DefaultFailurePolicy` | no | `FailHost` | The host-wide default for a module that declares none. |

### DatabaseConfig

```go
type DatabaseConfig struct {
    Engine           string          // e.g. "postgres"; empty means no shared DB
    DefaultIsolation IsolationPolicy // composition default for modules with Isolation unset
}
```

A standalone host leaves `HostConfig.Database` nil and runs its single module's migration through
`Migrate`. A composed suite host sets `Engine` so each module gets its own schema or table prefix
from its `DatabaseDescriptor`. See [Composable services](#composable-services).

### MigrationRunner

```go
type MigrationRunner func(ctx context.Context, ns DatabaseNamespace, d DatabaseDescriptor) error
```

`Run` calls the `MigrationRunner` once per module — after the module's `DatabaseNamespace` is
allocated and before the module registers — so the host, not the module, runs migrations. The runner
must be idempotent. The scaffold's `cmd/<svc>/main.go` supplies one backed by `gormtx.MigrateModule`
(GORM); the ent scaffold migrates with `client.Schema.Create` in `main` instead.

## Run

```go
func Run(hc HostConfig) error
```

`Run` is the host entrypoint. It builds the one shared server, registers every module on it, and
serves — the same path whether one module or several. It runs this sequence:

1. Resolve host defaults and the root context. The host owns signal handling; with `Context` nil,
   `Run` installs a context cancelled on `SIGTERM` or `Interrupt`.
2. Validate the composition: unique module IDs, no duplicate gRPC service names, no duplicate route
   prefixes, no duplicate permission names, and a coherent event graph (no orphan subscriber).
3. Resolve the shared backends once: the one server with its interceptor chain, the one event bus,
   the config store, the per-module database registry, and the supervisor.
4. For each module, in slice order: allocate its database namespace, run its migrations under a
   per-module advisory lock through `Migrate`, then call `Register` inside the module's panic
   boundary.
5. Start exactly one outbox relay and one consumer per module, plus every supervised background job,
   each in its module's bulkhead.
6. Call `server.Serve`. The server's fail-closed completeness gate validates the combined surface of
   every module, then blocks until the context is cancelled.
7. On shutdown, close the shared bus and wait for the supervised goroutines.

`Run` returns the first fatal error, or nil on clean shutdown. When a `FailHost` module takes the
host down, `Run` returns that module's failure cause rather than nil.

A minimal standalone host:

```go {filename="cmd/orders/main.go"}
func runHost(ctx context.Context, authorizer authz.Authorizer, grpcAddr, httpAddr, dsn string) error {
    repo, gormDB, err := newRepository(dsn) // open the DB, build the generated repository
    if err != nil {
        return err
    }
    sqlDB, err := gormDB.DB()
    if err != nil {
        return err
    }
    return servicekit.Run(servicekit.HostConfig{
        Modules:       []servicekit.Module{svcmodule.Module(repo, sqlDB)},
        GRPCAddr:      grpcAddr,
        HTTPAddr:      httpAddr,
        Migrate:       moduleMigrate(gormDB),
        Authorizer:    authorizer,
        PrincipalFunc: grpcauthz.DevPrincipalFunc(),
        Logger:        slog.Default(),
        Context:       ctx,
    })
}
```

## Registry interfaces

The host hands each module its registry seams through [`App`](#app). They are backed by real
per-module implementations.

```go
type ConfigProvider interface {
    Load(dst any) error
}

type DatabaseRegistry interface {
    Namespace(moduleID string, db DatabaseDescriptor) (DatabaseNamespace, error)
}

type EventRegistry interface {
    RegisterOutbox(moduleID string, d OutboxDescriptor) error
}

type HealthRegistry interface {
    Register(name string, check health.Check) error
}

type MetricsRegistry interface {
    Namespace(moduleID string) string
}
```

| Interface | What a module does with it |
|---|---|
| `ConfigProvider` (`app.Config`) | Loads the module's typed config struct, scoped to the module's prefix. Mirrors `config.Load`. |
| `DatabaseRegistry` (`app.DB`) | Resolves the module's `DatabaseNamespace`. A single-module or unshared-DB host yields a zero-qualification namespace. |
| `EventRegistry` (`app.Events`) | Declares the module's outbox. The richer relay and handler registration flows through `App.RegisterOutboxRelay` and `App.Subscribe`. |
| `HealthRegistry` (`app.Health`) | Adds a readiness check the host aggregates into `/readyz` and the gRPC health status. Use a module-qualified name. |
| `MetricsRegistry` (`app.Metrics`) | Returns a metric-safe per-module namespace token. |

`DatabaseNamespace` and `IsolationPolicy` are aliases of the `persistence` types of the same name —
the single source of truth shared with the gormtx and entrepo adapters that honor them.

## Failure policy

```go
type FailurePolicy string

const (
    FailurePolicyUnset FailurePolicy = ""          // defer to the host default
    FailHost           FailurePolicy = "fail-host" // a core module: a failure fails the host fast
    Degraded           FailurePolicy = "degraded"  // an optional module: a failure isolates the module
)
```

`Run` runs every module's `Register`, background jobs, and dispatchers inside an in-process bulkhead.
The `FailurePolicy` decides what a module failure does to the host:

- `FailHost` — a core module. A panic, a background-job crash, or a dispatcher failure cancels the
  host context, so the host fails fast. This is the conservative default and the standalone-friendly
  posture.
- `Degraded` — an optional module. A failure is isolated to that module: the module marks itself
  unready (its `/readyz` entry fails) and the host stays up.

The effective policy for a module is its `HostConfig.FailurePolicies` override, then its
`Descriptor.FailurePolicy`, then `HostConfig.DefaultFailurePolicy`, then `FailHost`.

## Composable services

The same module composes into a multi-service binary with no code change. A standalone host gives
`Run` one module; a suite host gives `Run` several, sharing one server, one process, one event bus,
and one database. Each module namespaces its own database tables under the host's shared engine, and
the host runs each module's migrations under its own advisory lock. The host owns one relay and one
consumer per module outbox, so a composed binary never double-starts a dispatcher.

The scaffold's `cmd/<svc>/main.go` is a standalone host. The `de compose` tooling builds a suite
host from several modules.

## See also

- [Add a custom method or second resource](../../how-to/model-and-persist/custom-methods/) — the
  task recipe built on this package.
- [server](../server/) — the shared `server.Server` `Run` builds.
- [codegen → protoc-gen-svc](../codegen/#protoc-gen-svc) — the generated `Module`, handler, and
  `Register<Service>` helpers.
- [Events](../../concepts/events/) — the outbox, relay, and consumer pipeline modules publish over.
