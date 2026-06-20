# F029 — Generated default CRUD service handlers (+ auto-wired registration & authz rules)

**AIPs**: AIP-131–135 (standard methods), AIP-132 (List), AIP-134 (Update/field-mask), AIP-148 (soft-delete/Undelete), AIP-137 (batch)
**Status**: CLARIFIED — decisions locked 2026-06-20 (ready for Plan)

> **Guiding principle (locked):** **clean implementation, NO backward compatibility.** The SDK is
> pre-1.0 with no real users — do not preserve superseded patterns. Rewrite the example/`testdata`
> services and the scaffold to the new approach; delete the hand-written handler delegations and any
> wiring the new path obsoletes. Correctness + clarity over migration safety.
**Extends**: F010 (`protoc-gen-svc`), F011 (service runtime / `server`), F027 (generated repository adapter), F028 (scaffold), WS-005 (multi-surface)
**Origin**: the `assetd`/`inventoryd` dogfood (Runs 10–11) — after F027 generates the repository and F028 scaffolds the service, the **last substantial hand-written file is the handler**, whose standard-method bodies are pure mechanical delegations to the generated `persistence.Repository`; plus the per-service `Register<Svc>` calls and the hand-assembled `server.Config{Rules: append(...)}` (finding #65). This feature generates all of it.

---

## Problem statement

Today `protoc-gen-svc` emits a clean `<Service>Server` **interface**, an `Unimplemented<Service>Server`, and `Register<Service>(s *server.Server, srv <Service>Server) error` (which wires gRPC + the REST gateway and runs the boot-time authz completeness gate). It does **not** generate a handler implementation. So for every service the developer hand-writes a handler whose standard methods are 1:1 delegations to the generated repository — verified in the `assetd` dogfood:

```go
func (h *handler) CreateAsset(ctx, req)  (*Asset, error) { return h.repo.Create(ctx, req.Asset) }            // repo already stamps tenant
func (h *handler) GetAsset(ctx, req)     (*Asset, error) { return h.repo.Get(ctx, req.Id) }
func (h *handler) ListAssets(ctx, req)   (*ListAssetsResponse, error) { … h.repo.List(ctx, ListOptions{PageSize, PageToken, Filter, OrderBy, ShowDeleted}) … }
func (h *handler) UpdateAsset(ctx, req)  (*Asset, error) { return h.repo.Update(ctx, req.Asset.Id, req.Asset, req.UpdateMask...) }
func (h *handler) DeleteAsset(ctx, req)  (*DeleteAssetResponse, error) { return … h.repo.Delete(ctx, req.Id) }
```

Nothing there is service-specific — it is fully derivable from the AIP method/request shapes `protoc-gen-svc` already parses + the `persistence.Repository[*R, K]` `protoc-gen-{ent,storage}` already generate. On top of that, the developer must (a) call `Register<Svc>` per service and (b) hand-collect every `<Svc>AuthzRules` into `server.Config{Rules: append(A, B...)}` — the #65 friction.

This is the same churn-elimination thesis as F027 (which cut the ~300-line hand-written `ent_wiring.go`), now applied to the handler — the ~80% "Tier-1 CRUD" case the vision targets ("the framework owns the generated CRUD/REST surface").

---

## Goals

- **G-1 (default handler).** `protoc-gen-svc` generates a `persistence.Repository`-backed default handler implementing the **AIP standard methods** it can detect (`Create`/`Get`/`List`/`Update`/`Delete`, plus `Undelete` when the resource is soft-delete and the batch methods when present), so a pure-CRUD service needs **no hand-written handler**. Output equivalence with today's hand-written delegations is the bar.
- **G-2 (one-call registration).** A generated `Register<Svc>WithRepository(s *server.Server, repo persistence.Repository[*R, K]) error` constructs the default handler and registers it into gRPC **and** the REST gateway — eliminating the per-service handler-construct + `Register` boilerplate for the CRUD case.
- **G-3 (auto-wired authz rules).** Registration contributes the service's `<Svc>AuthzRules` to the server automatically, so the developer never hand-assembles `server.Config{Rules: ...}` / `append(...)` (subsumes #65). The boot-time completeness gate still runs (fail-closed) over the accumulated rule set.
- **G-4 (first-class override / escape hatch).** Custom or non-CRUD logic is supported without giving up the generated default: either **embed** the generated default handler and override only the methods you change, or implement the bare `<Svc>Server` interface and use the existing `Register<Svc>(s, custom)`. The generated handler is `DO NOT EDIT`.
- **G-5 (no black box).** The generated handler is readable, conventionally mapped, `DO NOT EDIT`-marked, and the standard-method→repository mapping is documented. Custom (AIP-136) methods get clear `Unimplemented` stubs the developer fills.
- **G-6 (multi-surface parity).** A multi-surface projection service (`(infoblox.storage.v1.model)`) gets the same `Register<Surface>ServiceWithRepository` treatment over its surface repository.

## Non-goals

- **Owning `main.go` or the environment wiring.** Opening the DB, running migrations, constructing the repository (`New<R>EntRepository(client, enc)` / `New<R>Repository(db)`), and choosing the `Authorizer`/`PrincipalFunc` stay developer-owned in `main.go` — they are environment-specific and not derivable.
- **Generating custom-method bodies** (AIP-136 / non-standard RPCs) — those remain `Unimplemented` stubs.
- **Changing the `persistence.Repository` contract or the proto/API shape.** This feature only generates the handler that sits between the generated transport and the generated repository.
- A higher-level "infra-from-code" `Run()` entrypoint (Encore-style) that hides `main.go` — explicitly out of scope (the vision keeps `main.go` owned).

---

## Design decisions (★ = confirm in Clarify)

- **D-1 (where + shape).** The default handler is generated into the proto's Go package (`gen/<svc>v1`, `DO NOT EDIT`) as a struct embedding `Unimplemented<Svc>Server` and holding the repository, e.g.:
  ```go
  type <Svc>CRUDHandler struct {
      Unimplemented<Svc>Server
      Repo persistence.Repository[*<R>, string]
  }
  ```
  with one method per detected standard RPC. Override via Go embedding (`type handler struct { <pkg>.<Svc>CRUDHandler }` + redefine one method). Embedding `Unimplemented<Svc>Server` means an undetected/custom RPC is safely `Unimplemented` until provided.
- **D-1b (ergonomics — BOTH, locked).** Emit **both** entry points: `New<Svc>Handler(repo) *<Svc>CRUDHandler` (returns the default handler, so the developer can embed/wrap it before registering via the plain `Register<Svc>(s, h)`) **and** the convenience `Register<Svc>WithRepository(s, repo) error` (= `New<Svc>Handler` + `Register<Svc>` + contribute rules) for the one-call CRUD path.
- **D-2 (standard-method detection — id AND AIP-122 name, locked).** Detect standard methods by AIP request/response shape (not name alone): `Create<R>Request{ <R> }`→`repo.Create`; `Get<R>Request`/`Delete<R>Request` keyed by **`id` OR an AIP-122 `name`** (when the request carries `name`, parse it to the id via the generated `Parse<R>Name`)→`repo.Get`/`repo.Delete`; `List<R>sRequest{ page_size, page_token, filter?, order_by?, show_deleted? }`→`repo.List` + `…Response{ repeated <R>, next_page_token }`; `Update<R>Request{ <R>, update_mask }`→`repo.Update`; `Undelete<R>Request`→`repo.Undelete` (soft-delete only). Tolerate extra/optional request fields; an RPC that matches no known shape is left `Unimplemented`.
- **D-3 (registration + rules auto-wiring — BOTH, locked).** `Register<Svc>` (both variants) calls a new `server.Server.AddRules(<Svc>AuthzRules...)` so rules **accumulate from the Register calls**; the boot-time completeness gate moves to **`Serve()` over the accumulated set** (fail-closed preserved). **Also** emit a package aggregate `var AllAuthzRules = slices.Concat(<A>Rules, <B>Rules…)`. Because there is no back-compat constraint, `server.Config.Rules` becomes an **optional additive override** (merged with the accumulated set), not a required field — the generated path never needs it.
- **D-4 (ListOptions mapping).** Map the request's standard fields onto `persistence.ListOptions` (`PageSize`, `PageToken`, `Filter`, `OrderBy`, `ShowDeleted`) when present; `read_mask`/`field_mask` are applied by the existing interceptors, so the handler does not. Tenant stamping is the repository's job (it already stamps `account_id` from context), so the default `Create` does **not** duplicate it.
- **D-5 (clean rewrite — locked).** No back-compat. Rewrite the F028 scaffold template **and** the `testdata`/example services (`apikey`, `fleet`, `toy`, + an `assetd`-shape fixture) to the generated handler + `…WithRepository`, **deleting** the hand-written handler delegations and the now-obsolete `Config.Rules`/`append` wiring. Keep the bare-interface + `Register<Svc>` path only where a fixture deliberately exercises custom/non-CRUD logic (to prove the override escape hatch).
- **D-6 (scaffold).** The F028 scaffold's `server/main.go` template uses `Register<Svc>WithRepository(s, repo)` so a freshly scaffolded service has **zero** hand-written handler + zero rule wiring out of the box.

---

## Acceptance criteria

- **AC-1.** A single-resource service builds and serves full CRUD over gRPC **and** REST with **no hand-written handler** and **no hand-listed authz rules** — `main.go` is: build repo → `Register<Svc>WithRepository(s, repo)` → `s.Serve(ctx)`. Verified by a fixture re-running the `assetd` shape.
- **AC-2.** Overriding a single method via embedding works: a custom `Create<R>` plus the generated defaults for the rest compile and behave correctly; re-running codegen does not disturb the override (generated handler is `DO NOT EDIT`, override lives in the consumer's file).
- **AC-3.** A custom (non-CRUD, AIP-136) RPC returns `Unimplemented` from the default handler until the developer provides it; once provided (via embedding/override), it serves.
- **AC-4 (fail-closed preserved).** An RPC with neither an authz rule nor a `public: true` exemption still fails the boot-time completeness gate (now at `Serve`), and a service whose rules aren't contributed (e.g. never registered) cannot silently serve unprotected.
- **AC-5 (multi-surface).** An owner + a `(infoblox.storage.v1.model)` surface are both served via `…WithRepository` one-liners; both round-trip over the gateway on **ent and GORM**.
- **AC-6 (parity).** For a resource exercised by the existing fixtures, the generated default handler is behavior-equivalent to today's hand-written delegations (the existing apikey/fleet/toy runtime suites stay green against it).
- **AC-7 (docs).** `reference/codegen.md` documents the default handler + `…WithRepository` + the override/embedding escape hatch + the auto-wired rules; the scaffold + `guides/` show the one-liner path.

## Failure modes to cover

- **Mis-detection of a standard method** (a near-standard RPC wrongly auto-implemented, or a standard RPC wrongly left `Unimplemented`). Mitigation: conservative shape matching (D-2) + the override path; a non-matching method is `Unimplemented`, never silently wrong.
- **Auto-rules timing** (gate moves to `Serve`): a service registered but never served → gate never runs (acceptable — `Serve` is required to serve). A registered method lacking a rule/exemption → gate fails at `Serve` with a clear error.
- **Override foot-gun**: a consumer embeds the default but shadows a method with a wrong signature → compile error (good); forgetting `Unimplemented` embedding → handled because the generated default embeds it.
- **Repository/handler signature drift** (`persistence.Repository` shape vs. the generated handler) — render tests gate `go/format.Source` + the fixtures gate runtime behavior.
- **Non-resource / PK-less messages** (request/response wrappers, LRO `Operation`) must not get a handler (mirror the existing resource-detection rule).

---

## Phasing (to be detailed in tasks.md during Plan)

1. Standard-method shape detection in `protoc-gen-svc` (pure, unit-tested).
2. Render the `<Svc>CRUDHandler` + `Register<Svc>WithRepository` (+ optional `AllAuthzRules`); render tests (`go/format.Source` + substring).
3. `server.Server.AddRules` + move the completeness gate to `Serve`; keep `Config.Rules` honored (D-3a).
4. Migrate the F028 scaffold template + `testdata` fixtures to the one-liner; prove AC-1/2/3/5/6 on both backends.
5. Docs (AC-7) + a dogfood re-run (Run 12) building a CRUD service with zero hand-written handler.

---

## Resolved (Clarify — 2026-06-20)

All four forks decided (folded into Design decisions above):
1. **Rules wiring → BOTH** — auto-contribute via `server.AddRules` + gate at `Serve`, **and** emit the `AllAuthzRules` aggregate; `Config.Rules` is now an optional additive override (D-3).
2. **Ergonomics → BOTH** — `New<Svc>Handler(repo)` **and** `Register<Svc>WithRepository(s, repo)` (D-1b).
3. **Migration → clean rewrite, no back-compat** — rewrite the scaffold + `testdata` fixtures to the new approach and delete superseded wiring; no real users to preserve (D-5 + guiding principle).
4. **Detection → `id` AND AIP-122 `name`**, tolerant of extra request fields (D-2).

**Next gate:** Plan (`tasks.md`, tasks tagged `[S]`/`[C]`), then Implement.
