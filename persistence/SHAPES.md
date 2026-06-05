# Persistence shapes

**The vision drives the app and API shape; persistence serves it and is
pluggable.** The service framework is resource-oriented and AIP-aligned — the
API contract (resources, standard methods, field masks, filtering, pagination)
is primary. *How* a service stores those resources is a secondary, swappable
concern. This SDK therefore **does not impose an ORM** and has **no ORM
dependency**.

## What the SDK provides (shape-agnostic)

- **Connection convention** — the [`DSN`](./dsn.go) abstraction, including
  devedge's indirect *hotload* form (`fsnotify://<driver>/<abs-path>` + a real
  DSN file), so rotated credentials reload without a restart.
- **An optional neutral seam** — `Repository[T,K]` (get/list/create/update/delete,
  matching the API verb vocabulary) plus an in-memory `MemoryRepository` for the
  common CRUD case and for tests.
- **Migrations** — schema migrations use `infobloxopen/migrate` (the org-standard
  fork, already proven in devedge feature 006) regardless of shape.

## The shapes (a per-service, pluggable choice)

A "shape" is how entities and queries are modeled and generated. None is
mandated; a service picks the one that fits.

| Shape | Source of truth | What you get | When |
|---|---|---|---|
| **proto→GORM** (`protoc-gen-gorm` / a current-deps successor) | the `.proto` resource | generated ORM structs + CRUDL | the **current atlas approach** — one option, not the default; lowest-friction when the proto already defines the resource |
| **ent** ([entgo.io](https://entgo.io), Meta) | a Go ent schema | a generated, type-safe client; **graph edges/traversal**, hooks, and a **privacy layer** | rich domain graphs and relationship-heavy data; the privacy layer is interesting alongside the [authz](../authz) seam |
| **sqlc** | hand-written SQL | compile-time-safe query code, no reflection | hand-tuned queries / performance-critical paths (the "escape hatch") |
| **hand-written** | your code | implement `Repository[T,K]` directly | small/simple stores, or wrapping anything above |

## How a shape plugs in

Two valid styles, depending on how much of the shape's power you need:

1. **Behind the neutral seam** — wrap the generated client (gorm/ent/sqlc) in a
   type that implements `Repository[T,K]`. Service code depends only on the seam,
   so the shape can change locally without touching callers. Good for plain CRUD.
2. **Directly** — use the shape's generated client where its capabilities matter
   (e.g. ent's graph queries or privacy rules). The SDK does **not** force a
   lowest-common-denominator; it gives you the connection + migration conventions
   and gets out of the way.

## Status

The neutral seam, the in-memory dev store, and the DSN/hotload convention exist
today. Concrete shape adapters (a proto→GORM successor, an ent example, a sqlc
example) are follow-on work, chosen per service. This document is the contract
for keeping persistence pluggable as those land.
