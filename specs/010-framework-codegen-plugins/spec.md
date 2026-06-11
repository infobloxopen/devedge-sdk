# Feature Specification: Framework codegen plugins — protoc-gen-svc + protoc-gen-storage

**Feature Branch**: `010-framework-codegen-plugins`
**Created**: 2026-06-10
**Status**: Draft
**Phase**: W3–4 of the vision.md Phase-1 roadmap

## Context

W1–2 delivered the authz annotation contract (`infoblox.authz.v1.rule` in
`infobloxopen/apis`, consumed by `devedge-sdk`). W7–8 delivered the devedge scaffolder
(`de project init`). The W3–4 gap — the two `buf generate` plugins that turn a proto
service + resource definition into a compilable service skeleton — is the next step.

**Checkpoint (W3–4):** a toy service defined in proto compiles end-to-end: `buf generate`
runs both plugins, the generated `.svc.go` and `.storage.go` files compile as Go packages
against `devedge-sdk` and standard deps.

### What lives where

| Layer | Repo | Status |
|-------|------|--------|
| Layer 1 — substrate | `infobloxopen/devedge` | Done (features 001–009) |
| Layer 2 — SDK / plugins | `infobloxopen/devedge-sdk` | **This feature** |
| Layer 3 — canonical proto pipeline | `infobloxopen/apx` + `infobloxopen/apis` | Done (authz v1.0.0-alpha.2) |

### What the two plugins generate

**`protoc-gen-svc`** reads a proto `service` definition (with `infoblox.authz.v1.rule`
annotations) and emits `<name>.svc.go`:
- A clean application-layer handler interface (`<Service>Server`) — methods take/return
  proto types directly, no gRPC plumbing exposed.
- An `Unimplemented<Service>Server` embed-stub returning `codes.Unimplemented`.
- A `Register<Service>` function wiring the handler into a `*grpc.Server` via an
  internal adapter that implements the proto-generated `pb.<Service>Server` interface.
- The file carries a `DO NOT EDIT` header and a `.svc.` infix in its name.

**`protoc-gen-storage`** reads a proto `message` definition and emits `<name>.storage.go`:
- A GORM model struct (`<Message>Model`) with snake_case columns derived from proto field
  names, a UUID primary key, `CreatedAt`/`UpdatedAt`/`DeletedAt` (soft-delete via
  `gorm.DeletedAt`), and an `ETag` column (optimistic concurrency — decided now per
  vision.md Part 6 decision #2).
- A `<Message>Repository` implementing
  `persistence.Repository[*pb.<Message>, string]`.
- `New<Message>Repository(db *gorm.DB)` constructor.
- Proto↔model conversion helpers (`toModel`, `fromModel`).
- Compile-time `var _ persistence.Repository[...] = (*<Message>Repository)(nil)` check.
- The file carries a `DO NOT EDIT` header and a `.storage.` infix.

### What is NOT in scope for W3–4

- grpc-gateway registration (W5–6 runtime wiring)
- Secret field enforcement (W5–6)
- ETag/`If-Match` middleware (W5–6 — the model *column* is generated now; the interceptor
  logic is W5–6)
- New annotation proto beyond `infoblox.authz.v1.rule` (storage hints and field options are
  W5–6; for W3–4 we derive everything from proto naming conventions)
- `security-check` tool (W9–10)
- Eventing / outbox (W5–6+)

## Clarifications

- **GORM dep scope.** `gorm.io/gorm` is NOT added to `devedge-sdk`'s `go.mod`. The plugin
  binary only generates text; GORM is imported by the generated code. The consumer service's
  `go.mod` provides GORM. The compilation test (`testdata/toy/`) has its own minimal
  `go.mod` with `gorm.io/gorm` (a lightweight self-contained module used only for the test).
- **Handler interface name.** The generated type is `<Service>Server` (matching the
  proto-generated `pb.<Service>Server` convention but distinct from it — our type is the
  clean app-layer interface; the adapter is unexported and bridges the two).
- **Resource ID convention.** For W3–4, the primary key is the proto field named `id`
  (type `string`). AIP resource names (`parent/collection/id` compound names) are a W5–6
  concern; the storage layer uses bare IDs for now.
- **ListOptions → GORM.** `persistence.ListOptions.Filter` is passed to GORM's
  `Where(filter)` verbatim for W3–4 (unsafe but functional for dev/test; AIP-160 AST
  parsing is W5–6).
- **Field type mapping.** Proto scalar types map to Go types per the standard proto3 Go
  mapping. Enum fields become `int32`. Nested message fields are skipped by
  `protoc-gen-storage` for W3–4 (they don't have a clean GORM column mapping without
  serialization decisions — add a TODO comment in the generated output).
- **Buf integration.** Both plugins are added to `devedge-sdk`'s `buf.gen.yaml` and built
  to `./bin/` by `make generate` alongside the existing `protoc-gen-devedge-authz`.
- **Plugin binary names.** `protoc-gen-svc` and `protoc-gen-storage` (standard protoc
  plugin naming).

## User Scenarios & Testing

### User Story 1 — `buf generate` on a proto service emits a compilable handler interface (P1) 🎯

A developer writes a proto service with authz annotations and runs `buf generate`. The
plugin emits a `.svc.go` file with a handler interface, unimplemented stub, and gRPC
registration function. The generated package compiles with `go build`.

**Acceptance Scenarios**:

1. **Given** a proto `service Foo` with two RPCs each carrying `(infoblox.authz.v1.rule)`,
   **When** `protoc-gen-svc` runs, **Then** `foo.svc.go` is emitted containing:
   a `FooServer` interface with those two methods, an `UnimplementedFooServer` struct, and
   a `RegisterFoo(*grpc.Server, FooServer)` function.
2. **Given** the generated `foo.svc.go`, **When** `go build ./...` runs on the test package,
   **Then** it compiles without errors.
3. **Given** a proto file with NO `(infoblox.authz.v1.rule)` annotations on any method,
   **When** `protoc-gen-svc` runs, **Then** no `.svc.go` file is emitted (no empty output).

**Independent Test**: plugin unit test with synthetic proto descriptors + generated-output
string assertions; compilation test via `go build` on `testdata/toy/`.

---

### User Story 2 — `buf generate` on a proto message emits a compilable GORM repository (P1)

A developer writes a proto message for their resource and runs `buf generate`. The plugin
emits a `.storage.go` file with a GORM model and a `persistence.Repository` implementation
that compiles.

**Acceptance Scenarios**:

1. **Given** a proto `message Bar` with scalar fields including one named `id`,
   **When** `protoc-gen-storage` runs, **Then** `bar.storage.go` is emitted containing
   a `BarModel` GORM struct, a `BarRepository` struct, `NewBarRepository(*gorm.DB)`,
   and CRUD methods matching `persistence.Repository[*pb.Bar, string]`.
2. **Given** the generated `bar.storage.go`, **When** `go build ./...` runs on the test
   package (with gorm in its go.mod), **Then** it compiles without errors.
3. **Given** a `BarModel` generated from `message Bar { string id = 1; string name = 2; }`,
   **When** inspecting the struct, **Then** it has `ID string` (primaryKey), `Name string`
   (column:name), `ETag string` (column:etag), `CreatedAt time.Time`, `UpdatedAt time.Time`,
   `DeletedAt gorm.DeletedAt`.
4. **Given** a `BarRepository`, **When** calling `var _ persistence.Repository[*pb.Bar, string] = (*BarRepository)(nil)`,
   **Then** it compiles (static interface satisfaction check).

**Independent Test**: plugin unit test with synthetic descriptors + struct shape assertions;
compilation test via `go build` on `testdata/toy/`.

---

### User Story 3 — Both plugins run together in `buf generate` on the same proto file (P1)

A developer writes a single proto file containing both a service and a message, runs
`buf generate`, and both plugins fire. The combined output compiles.

**Acceptance Scenarios**:

1. **Given** a proto file with `message Widget` and `service WidgetService` (both annotated),
   **When** `buf generate` runs with both plugins configured, **Then** `widgets.svc.go`
   AND `widgets.storage.go` are emitted in the correct output directory.
2. **Given** both generated files, **When** `go build ./...` on the combined test package,
   **Then** it compiles — the handler interface and the repository exist in the same package.

**Independent Test**: end-to-end `buf generate` run on `testdata/toy/widgets.proto` followed
by `go build ./testdata/toy/...`.

---

### Edge Cases

- A proto `service` with zero RPCs → `protoc-gen-svc` emits no file (or an empty interface
  with a comment).
- A proto `message` with no scalar fields (all nested messages) → `protoc-gen-storage` emits
  a model with only metadata columns (ID, ETag, timestamps) and a TODO comment for each
  skipped field.
- A `repeated` field in the proto message → skipped by `protoc-gen-storage` for W3–4 (JSONB
  storage decision deferred; TODO comment emitted).
- Plugin receives a proto file not in the `generate` set → skipped (standard protogen
  behavior).

## Requirements

### Functional Requirements

- **FR-001**: `protoc-gen-svc` MUST emit a `<name>.svc.go` file (with `DO NOT EDIT` header
  and `.svc.` infix) for every proto file that contains at least one service with
  `(infoblox.authz.v1.rule)` annotations.
- **FR-002**: The generated `<Service>Server` interface MUST contain one method per RPC in
  the proto service, with the exact same request/response types.
- **FR-003**: `RegisterFoo(s *grpc.Server, srv FooServer)` MUST register an adapter that
  delegates all calls to `srv`.
- **FR-004**: `protoc-gen-storage` MUST emit a `<name>.storage.go` file (with `DO NOT EDIT`
  header and `.storage.` infix) for every proto file that contains at least one message.
- **FR-005**: The generated GORM model MUST include: `ID string` (primaryKey), one column
  per proto scalar field (snake_case name), `ETag string` (column:etag), `CreatedAt`,
  `UpdatedAt`, `DeletedAt gorm.DeletedAt`.
- **FR-006**: The generated `<Message>Repository` MUST satisfy
  `persistence.Repository[*pb.<Message>, string]` at compile time.
- **FR-007**: Both plugins MUST be added to `devedge-sdk/buf.gen.yaml` and buildable via
  `make generate`.
- **FR-008**: Neither plugin binary MUST NOT add `gorm.io/gorm` or any ORM to
  `devedge-sdk`'s `go.mod`.

### Key Entities

- **`<Service>Server`** (generated): clean app-layer handler interface; input to
  `Register<Service>`.
- **`<Message>Model`** (generated): GORM model struct; internal to the storage package.
- **`<Message>Repository`** (generated): GORM-backed implementation of
  `persistence.Repository[*pb.<Message>, string]`.

## Success Criteria

- **SC-001**: `buf generate` on `testdata/toy/widgets.proto` emits `widgets.svc.go` and
  `widgets.storage.go` without errors.
- **SC-002**: `go build ./testdata/toy/...` passes (toy package has `gorm.io/gorm` in its
  own `go.mod`).
- **SC-003**: `go build ./...` and `go vet ./...` on `devedge-sdk` pass without adding
  `gorm.io/gorm` to `devedge-sdk`'s `go.mod`.
- **SC-004**: Unit tests for both plugins pass: field mapping, handler interface shape,
  empty-annotation skip behavior.
- **SC-005**: Both plugin binaries build via `make generate` (`go build ./cmd/protoc-gen-svc`
  and `go build ./cmd/protoc-gen-storage`).
