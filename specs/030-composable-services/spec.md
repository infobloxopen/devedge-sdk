# 030 — Composable Services: importable Module + executable Host (`servicekit`)

**Status**: P1 implemented (this commit). P2–P6 planned.
**Initiative**: WS-012 (cross-repo proposal: development-hub `specs/composable-services-proposal.md`).
**Decisions (ratified P0):** package `servicekit` in the ROOT module; generated Module via
`protoc-gen-svc`; static composition only (no Go plugins); one shared `server.Server`;
events-only cross-module coupling; no cross-module FKs; schema-preferred DB default (P2).

This is the **SDK-side umbrella** for the whole feature; later phases append to this spec.

## Problem

The SDK already runs many services in one process by hand (`server.New` once →
`Register<Svc>WithRepository(s, repo)` per service → `s.Serve(ctx)`; the boot-time **union
completeness gate** at `Serve` validates the combined surface). What is missing is an
**importable, self-describing unit** so a service can be composed without hand-merging
`main.go`s — and the boundary discipline that keeps services separable when co-resident.

## Decision — the module / host split

Split "a service" into two artifacts:

- a **Module** (importable library) that owns **domain behavior**, and
- one or more **Hosts** (executables) that own **process behavior**.

The **same module** runs **standalone** (one module per host) or **composed** into a "suite"
binary (N modules, one host) by changing the *host*, not the *module*.

### "Module owns domain, host owns process" — the rule list

A module **MAY** own (domain): resources, handlers, repositories, migration **files**, event
publishes/subscribes, config **schema**, health checks, background jobs, authz rules, routes,
and its self-describing `Descriptor`.

A module **MUST NOT** own (process — the host's job): process lifetime, listen ports, global
flags / global env / global logging config, **DB connection/creation** outside its namespace,
listener startup, signal handling, `os.Exit` / `log.Fatal`, running its own migrations from
`init()`, or any cross-module table ownership / cross-module foreign key.

## The contract (P1 — `servicekit`, root module)

```go
type Module interface {
    Descriptor() Descriptor                       // static, introspectable before boot
    Register(ctx context.Context, app *App) error // wire into the shared host
}

type Descriptor struct {
    ID, DisplayName, Version string
    Methods        []string            // gRPC FullMethods (from the generator)
    AuthzRules     []authz.MethodRule  // the generated <Svc>AuthzRules
    Routes         []RouteDescriptor   // HTTP prefixes / hostnames
    Resources      []ResourceDescriptor
    Config         ConfigDescriptor    // typed schema + prefix (P3)
    Database       DatabaseDescriptor  // isolation policy + migrations FS (P2)
    Events         EventDescriptor     // publishes/subscribes + outbox (P3)
    HealthChecks   []HealthDescriptor
    BackgroundJobs []JobDescriptor
    Requires       Compatibility       // sdk/go/postgres ranges (P4/P5 gating)
}

type App struct {                       // the running host's shared services
    Server  *server.Server              // the ONE shared server (server.New)
    Config  ConfigProvider              // per-module config (P3 prefix scoping)
    DB      DatabaseRegistry            // namespaced DB handle (P2)
    Events  EventRegistry               // host-owned relay/consumer per module (P3)
    Health  HealthRegistry              // registers checks on the shared server (P1: live)
    Logger  *slog.Logger
    Metrics MetricsRegistry             // per-module metrics (P3)
}

func Run(HostConfig) error              // build server ONCE, register each module, Serve
type HostConfig struct {
    Modules []Module
    GRPCAddr, HTTPAddr string
    Authorizer authz.Authorizer
    PrincipalFunc grpcauthz.PrincipalFunc
    Logger *slog.Logger
    ConfigSources []config.Source
    Context context.Context             // nil => host installs SIGTERM/Interrupt handler
}
```

`Run`'s P1 lifecycle: resolve host defaults + root context (host owns signals) →
`ValidateModules` (unique IDs; no duplicate gRPC service names / route prefixes / permission
names) → `server.New(cfg)` **once** → for each module `module.Register(ctx, app)` → `server.Serve(ctx)`.
The fail-closed **union completeness gate is reused** from `server.Serve` — `Run` does not
invent a parallel gate. DB namespacing (P2), host-owned relays / config layering / bulkheads /
failurePolicy (P3) are explicit extension points marked `P3:` in `run.go`.

### Generated Module (protoc-gen-svc)

For a CRUD service, the generator emits, alongside the existing `Register<Svc>` /
`Register<Svc>WithRepository` (unchanged), a `<Svc>Module(<Svc>ModuleOptions) servicekit.Module`
whose `Descriptor()` is the proto facts (module ID = proto-package first segment; methods; the
generated `<Svc>AuthzRules`; module-qualified resource name) and whose `Register` wraps
`Register<Svc>WithRepository(app.Server, opts.Repo)`. Hand-written extras flow through
`<Svc>ModuleOptions` (P1: the repo; later: health/events/jobs/handler overrides).

### Scaffold (`devedge-sdk new service`) — composable shape

The scaffold emits an importable `module/` package (`module/module.go` exposing `Module(repo,
db)` + the embedded `module/migrations/` placeholder) and a thin `cmd/<svc>/main.go` host that
does the process wiring (flags/env/config, DB open + migrate, otel, signals) then calls
`servicekit.Run` with the one module. The module registers the DB readiness check via the
Health registry, so `/readyz` behavior is unchanged.

## Acceptance criteria

- **AC-1 (contract):** `servicekit` defines `Module`, `Descriptor` (+ the descriptor sub-types),
  `App`, the five registry interfaces, and `Run(HostConfig)`; it imports only root SDK packages
  + `log/slog` and builds with `GOWORK=off`. ✅
- **AC-2 (validation):** `ValidateModules` rejects duplicate module IDs, duplicate gRPC service
  names, duplicate route prefixes, and duplicate permission names; it does **not** duplicate the
  rule-completeness gate. ✅
- **AC-3 (run, 1 and N):** `Run` serves a single module and trivially N modules; an undeclared
  method fails closed via the **existing** `server.Serve` gate. ✅
- **AC-4 (generated Module):** the generator emits `<Svc>Module`/`<Svc>ModuleOptions` for a CRUD
  service, with the descriptor populated from proto facts and `Register` wrapping
  `Register<Svc>WithRepository`; it does **not** replace the existing registrars. ✅
- **AC-5 (fixture exercises the shape):** at least one testdata fixture (toy) constructs the
  generated Module and serves it end-to-end through `servicekit.Run`. ✅
- **AC-6 (no behavior change, standalone):** a regenerated scaffold builds and serves
  identically — `make generate && make build && make test` green for gorm/ent and the aggregate
  variants; the fail-closed authz gate still triggers at boot through `runHost` → `servicekit.Run`. ✅
- **AC-7 (scope):** every change traces to P1 scope; P2/P3 stubs (DatabaseDescriptor/Namespace,
  inert DB/Event/Metrics registries, config layering) are inert and marked. ✅

## Migration note (existing standalone services)

P1 changes **no runtime behavior**; it formalizes the unit. To adopt the composable shape in an
existing service generated by an earlier scaffold:

1. `make generate` to pick up the generator's new `<Svc>Module()` (additive — the existing
   `Register<Svc>`/`Register<Svc>WithRepository` are unchanged, so a service that keeps calling
   them directly needs no change at all).
2. Optionally move `server/main.go` → `cmd/<svc>/main.go` and add a `module/module.go` that
   returns `Module(repo, db)`; replace the in-line `server.New(...) + Serve(...)` with
   `servicekit.Run(servicekit.HostConfig{Modules: []servicekit.Module{module.Module(repo, db)}, …})`.
   Behavior is identical (same server config, same DB readiness check, same boot gate).
3. Update `Makefile` `run:` and the smoke test to the `cmd/<svc>` path if you moved `main`.

A service that does nothing keeps working — the change is opt-in until P3/P4 composition is wanted.

## Phase checklist

- [x] **P1 — Module contract + generated `Module()` + composable scaffold.** `servicekit`
  package; `protoc-gen-svc` emits `Module()`/`Descriptor()`; scaffold emits `module/` + thin
  `cmd/<svc>/main.go`. Standalone binaries keep working. (this commit)
- [ ] **P2 — DB module-namespacing.** `DatabaseNamespace` allocation; namespaced framework +
  domain tables (outbox / idempotency / migrations); host-run, advisory-locked per-module
  migrations; no-cross-module-FK rule. Postgres schema isolation + prefix fallback tests.
- [ ] **P3 — Composition host.** Enrich `servicekit.Run`: shared backends resolved once,
  host-owned relay/consumer per module outbox, config prefix layering, in-process bulkheads +
  `failurePolicy`. Full ConfigProvider / DatabaseRegistry / EventRegistry / MetricsRegistry impls.
- [ ] **P4 — `kind: Composition` + `de compose`** (devedge side): resource kind +
  `de compose init/add/tidy/build/test/up`; generate `cmd/<comp>/main.go` + `composition.lock`.
- [ ] **P5 — Composition test harness.** `servicekittest.AssertModule` + `AssertComposition`;
  version-compatibility gating.
- [ ] **P6 — Deployment rendering.** `de compose chart` → single-binary / multi-daemon / hybrid
  from one descriptor set (reuse WS-007 Helm/Flux/Compose seam).

## Files (P1)

- `servicekit/{servicekit,registry,validate,run,config}.go` + `servicekit/servicekit_test.go`
- `server/server.go` — `AddReadinessCheck` (live append, mirrors `AddRules`)
- `cmd/protoc-gen-svc/{render,main}.go` + `render_test.go` — `renderModule` + proto-package threading
- `cmd/devedge-sdk/internal/scaffold/templates/{module.go,migrations.README.md,main.go,main.ent.go,smoke_test.go,Makefile,README.md}.tmpl`
  + `render.go` (new outputs) + the integration/deploy test path updates
- `testdata/{toy,apikey,iam}/…svc.go` (regenerated) + `testdata/toy/module_test.go`
