# Implementation Plan: Framework codegen plugins — protoc-gen-svc + protoc-gen-storage

**Branch**: `010-framework-codegen-plugins` | **Date**: 2026-06-10 | **Spec**: `specs/010-framework-codegen-plugins/spec.md`

## Summary

Two new `protoc` plugins in `devedge-sdk`:
- `cmd/protoc-gen-svc` — proto service → handler interface + gRPC registration glue (`.svc.go`)
- `cmd/protoc-gen-storage` — proto message → GORM model + `persistence.Repository` impl (`.storage.go`)

Both follow the `cmd/protoc-gen-devedge-authz` pattern (use `google.golang.org/protobuf/compiler/protogen`).
Checkpoint: a toy proto at `testdata/toy/widgets.proto` runs through `buf generate` and
`go build ./testdata/toy/...` passes.

## Technical Context

**Language/Version**: Go (devedge-sdk module, go 1.25.5)
**New packages**:
- `cmd/protoc-gen-svc/` — plugin binary
- `cmd/protoc-gen-storage/` — plugin binary
- `testdata/toy/` — toy proto + standalone go.mod with gorm dep for compilation test

**New direct dep in devedge-sdk**: none (`gorm.io/gorm` lives only in `testdata/toy/go.mod`)
**Affected files**: `buf.gen.yaml` (add both plugins), `Makefile` (add build targets)

**Testing**: unit tests via synthetic `protogen.Plugin` invocation (no buf needed); integration
test runs `buf generate` + `go build` in a subprocess.

## Clean-Core Invariant

Per `CLAUDE.md`: "No ORM in the core packages." These are code-generation tools — dev-time
binaries. The generated output imports GORM; the plugin binary does not. `go.mod` for the
`devedge-sdk` module gains no new ORM dependency. ✓

## Implementation Design

### `protoc-gen-svc` logic

```
for each proto file marked for generation:
  for each service in the file:
    collect methods annotated with (infoblox.authz.v1.rule)
    if no methods found → skip this service
  if no services with annotations → skip this file

  emit "<filename>.svc.go" to the same Go package as the .pb.go:
    package <pkg>
    
    // <Service>Server is the app-layer handler interface.
    type <Service>Server interface {
        <Method>(ctx, *req) (*resp, error)
        ...
    }
    
    // Unimplemented<Service>Server returns codes.Unimplemented.
    type Unimplemented<Service>Server struct{}
    func (Unimplemented<Service>Server) <Method>(context.Context, *req) (*resp, error) {
        return nil, status.Error(codes.Unimplemented, "<Method> not implemented")
    }
    
    // Register<Service> wires srv into s.
    func Register<Service>(s *grpc.Server, srv <Service>Server) {
        pb.Register<Service>Server(s, &<service>Adapter{srv: srv})
    }
    
    type <service>Adapter struct{ srv <Service>Server }
    func (a *<service>Adapter) <Method>(ctx, req) (resp, error) { return a.srv.<Method>(ctx, req) }
```

Notes:
- The adapter implements the proto-generated `pb.<Service>Server` interface.
- For W3–4, the adapter is a pure passthrough. W5–6 will inject middleware (authz check,
  request-id, tracing) into the adapter body.
- Methods NOT annotated with `(infoblox.authz.v1.rule)` are skipped from the interface
  (they'll be picked up by `protoc-gen-devedge-authz` logic at W5–6 as authz-missing errors).
  For W3–4: include ALL service methods regardless of annotation (fail-closed wiring is W5–6).

### `protoc-gen-storage` logic

```
for each proto file marked for generation:
  for each message in the file:
    collect scalar fields (skip: repeated, message, map, oneof members)
    identify the "id" field (field named "id", type string)
    if no id field found → emit a TODO and use first string field as key

  emit "<filename>.storage.go" in a sub-package "<pkg>storage":
    package <pkg>storage
    
    // <Message>Model is the GORM model.
    type <Message>Model struct {
        ID        string         `gorm:"primaryKey;type:varchar(36)"`
        <Field>   <GoType>       `gorm:"column:<snake_name>"`
        ...
        ETag      string         `gorm:"column:etag"`
        CreatedAt time.Time
        UpdatedAt time.Time
        DeletedAt gorm.DeletedAt `gorm:"index"`
    }
    
    // toModel converts proto → GORM model.
    func toModel(p *pb.<Message>) *<Message>Model { ... }
    
    // fromModel converts GORM model → proto.
    func fromModel(m *<Message>Model) *pb.<Message> { ... }
    
    // <Message>Repository implements persistence.Repository[*pb.<Message>, string].
    type <Message>Repository struct { db *gorm.DB }
    func New<Message>Repository(db *gorm.DB) *<Message>Repository { ... }
    
    func (r *<Message>Repository) Get(ctx, key) (*pb.<Message>, error) { ... }
    func (r *<Message>Repository) List(ctx, opts) ([]*pb.<Message>, string, error) { ... }
    func (r *<Message>Repository) Create(ctx, entity) (*pb.<Message>, error) { ... }
    func (r *<Message>Repository) Update(ctx, key, entity, mask...) (*pb.<Message>, error) { ... }
    func (r *<Message>Repository) Delete(ctx, key) error { ... }
    
    var _ persistence.Repository[*pb.<Message>, string] = (*<Message>Repository)(nil)
```

Notes:
- Output package is `<pkg>storage` (e.g., `widgetsv1storage`) to avoid collision with the
  proto-generated `<pkg>` package. Output file path: same directory + `<name>.storage.go`.
- `List`: applies `opts.Filter` as a GORM `Where` clause (verbatim for W3–4), `opts.PageSize`
  as `Limit`, and encodes an offset-based `NextPageToken` as a base64 int (simple, replaceable
  at W5–6 with a cursor-based scheme).
- `Update`: applies `fieldMask` to limit which columns are updated; empty mask = all columns.
- `Delete`: sets `DeletedAt` via `db.Delete` (GORM soft-delete).
- Nested message fields → skipped with a `// TODO: nested message <FieldName> skipped` comment.
- Repeated fields → skipped with a `// TODO: repeated field <FieldName> skipped` comment.

### Toy test proto

`testdata/toy/widgets.proto`:
```protobuf
syntax = "proto3";
package toy.v1;
option go_package = "github.com/infobloxopen/devedge-sdk/testdata/toy/widgetsv1;widgetsv1";

import "infoblox/authz/v1/authz.proto";
import "google/api/annotations.proto";  // needed for http option; kept minimal

message Widget {
  string id     = 1;
  string name   = 2;
  string color  = 3;
  int32  weight = 4;
}

message CreateWidgetRequest { Widget widget = 1; }
message GetWidgetRequest    { string id = 1; }
message ListWidgetsRequest  { int32 page_size = 1; string page_token = 2; }
message ListWidgetsResponse { repeated Widget widgets = 1; string next_page_token = 2; }
message UpdateWidgetRequest { Widget widget = 1; repeated string update_mask = 2; }
message DeleteWidgetRequest { string id = 1; }
message DeleteWidgetResponse {}

service WidgetService {
  rpc CreateWidget(CreateWidgetRequest) returns (Widget) {
    option (infoblox.authz.v1.rule) = { verb:"create" resource:"widgets" };
  }
  rpc GetWidget(GetWidgetRequest) returns (Widget) {
    option (infoblox.authz.v1.rule) = { verb:"read" resource:"widgets" };
  }
  rpc ListWidgets(ListWidgetsRequest) returns (ListWidgetsResponse) {
    option (infoblox.authz.v1.rule) = { verb:"read" resource:"widgets" };
  }
  rpc UpdateWidget(UpdateWidgetRequest) returns (Widget) {
    option (infoblox.authz.v1.rule) = { verb:"update" resource:"widgets" };
  }
  rpc DeleteWidget(DeleteWidgetRequest) returns (DeleteWidgetResponse) {
    option (infoblox.authz.v1.rule) = { verb:"delete" resource:"widgets" };
  }
}
```

`testdata/toy/go.mod`: a standalone Go module with `gorm.io/gorm`, `google.golang.org/grpc`,
`google.golang.org/protobuf`, and `github.com/infobloxopen/devedge-sdk` as deps.

### Test approach

**Unit tests** (no buf/cluster needed):
- `cmd/protoc-gen-svc/main_test.go`: drive `generateFile` with synthetic `*protogen.File`
  (built from the testpb fixture already in `authzpb/internal/testpb`); assert:
  - Output file path ends in `.svc.go`
  - Contains `type NotesServiceServer interface`
  - Contains `RegisterNotesService`
  - Contains `UnimplementedNotesServiceServer`
  - Skips a service with no annotations
- `cmd/protoc-gen-storage/main_test.go`: drive `generateFile` with a synthetic message;
  assert:
  - Output ends in `.storage.go`
  - Contains `type NoteModel struct`
  - Contains `gorm:"primaryKey"`
  - Contains `gorm:"column:etag"`
  - Contains `persistence.Repository`
  - Nested/repeated fields produce TODO comments

**Integration test** (requires `buf` + `go` CLI):
- `cmd/protoc-gen-svc/integration_test.go` (build tag `integration`):
  `go build ./cmd/protoc-gen-svc` → buf generate → `go build ./testdata/toy/...`
  (run with `make test-integration`)

## File Map

| File | Change |
|------|--------|
| `cmd/protoc-gen-svc/main.go` | New — plugin binary |
| `cmd/protoc-gen-svc/main_test.go` | New — unit tests |
| `cmd/protoc-gen-storage/main.go` | New — plugin binary |
| `cmd/protoc-gen-storage/main_test.go` | New — unit tests |
| `testdata/toy/widgets.proto` | New — toy proto |
| `testdata/toy/go.mod` | New — standalone module with gorm |
| `testdata/toy/go.sum` | New — lockfile |
| `buf.gen.yaml` | Add both plugins |
| `Makefile` | Add `protoc-gen-svc`, `protoc-gen-storage` build targets |

## Ordering

1. Extend test proto (`authzpb/internal/testpb`) to include a `NoteStorage` message (reuse
   the existing proto to avoid a new dep); write unit test stubs (red).
2. Implement `protoc-gen-svc` → unit tests green.
3. Implement `protoc-gen-storage` → unit tests green.
4. Write toy proto (`testdata/toy/`) + standalone `go.mod`.
5. Update `buf.gen.yaml` + `Makefile`.
6. Run `buf generate` on toy proto → check generated files.
7. Run `go build ./testdata/toy/...` — SC-002.
8. Run `go build ./...` + `go vet ./...` on the main module — SC-003.
