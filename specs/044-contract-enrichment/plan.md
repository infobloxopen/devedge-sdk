# Implementation Plan: 044 contract enrichment

**Spec**: `spec.md` (this dir). **Branch**: `044-contract-enrichment`.

## Architecture

Two coupled parts on one new seam. The seam is a shared, generator-agnostic package that
resolves AIP facts from **`protoreflect` descriptors** — the common denominator between the
`protogen` types protoc plugins use (`protogen.Field.Desc`, `protogen.Method.Desc`) and the
`FileDescriptorSet` the OpenAPI enrichment pass reads (`descriptorpb` →
`protodesc.NewFiles` → `protoreflect.FileDescriptor`). Both paths import the same package, so
a service's compiled behavior and its published OpenAPI cannot drift (D-new-1).

```
                         internal/aip  (NEW, protoreflect-only, stdlib+protobuf deps)
       ┌── ResolveFieldBehavior(fd protoreflect.FieldDescriptor) ([]FieldBehavior, error)   [Part A]
       ├── ClassifyMethod(md protoreflect.MethodDescriptor, res ...) StdMethod              [moved]
       ├── DetectResource / ResourceIdentity(md protoreflect.MessageDescriptor)             [moved]
       └── (contradiction lint lives inside ResolveFieldBehavior — fail-loud)
              ▲                                    ▲
   protoc-gen-svc / -storage / -ent          cmd/openapiv2to3 (enrichment pass)
   (Part A: re-point 3 read sites)           (Part B: reads toy.binpb FDS, writes
   + middleware/redact (INPUT_ONLY)           native readOnly/writeOnly/required/enum
                                              + x-aip-* onto the openapi3.T)
```

## Key decisions (implementation-level)

- **`internal/aip` operates on `protoreflect`, not `protogen`.** `classifyMethod` /
  `detectServiceResource` currently in `cmd/protoc-gen-svc/main.go` (`package main`,
  operating on `*protogen.Method`) are rewritten to take `protoreflect.MethodDescriptor` /
  `MessageDescriptor` and moved to `internal/aip`. The svc plugin calls them with `m.Desc` /
  `msg.Desc`. Behavior for existing fixtures must be identical (golden-diff proves it).
- **Dependency isolation.** `internal/aip` imports only `google.golang.org/protobuf/...`
  (protoreflect, descriptorpb, protodesc) + the `google.golang.org/genproto/googleapis/api/annotations`
  (field_behavior, resource) already used, + `infoblox.field.v1` (`fieldv1`) already vendored.
  No new heavy deps → `check-graph-isolation.sh` stays green. kin-openapi is already a dep for
  the enrichment side.
- **FDS production.** Add a `buf build -o testdata/toy/toy.binpb` step to `make generate`
  (before `openapiv2to3`). `openapiv2to3` gains a 3rd arg (or `--descriptor` flag) for the FDS
  path; unmarshalled exactly like `cmd/security-check/main.go:32-51`.
- **Consumer-neutral extensions** (D-new-3): native OpenAPI fields where they exist; else
  `x-aip-field-behavior` / `x-aip-resource` / `x-aip-method` / `x-aip-pagination` /
  `x-aip-references`. No `x-terraform-*`.
- **No proto schema change.** `field_behavior` already exists in `google/api`; we start using
  its full range. No new `infoblox.field.v1` release; scaffold mirror stays byte-identical.

## Test strategy (functional + e2e; the QA gate)

- Unit: `internal/aip` resolver table (explicit, derived, contradiction→error, not_null NOT
  REQUIRED) + classifier parity on the toy descriptors.
- Golden: `make generate && git diff --exit-code` clean; enriched `toy.openapi.yaml` asserts
  every native field + `x-aip-*`.
- **e2e (real boundary)**: boot the toy server, issue a real REST create/get, assert an
  `INPUT_ONLY`/`secret` field is stripped from the response (extends the existing toy
  security/redact test — this is the runtime boundary, not just codegen).
- Drift golden (AC-6): feed the same toy FDS to the generator classification and the
  enrichment classification; assert identical.
- Failure fixtures: a contradictory-annotation proto aborts codegen with a named error (FM-1);
  missing FDS aborts `openapiv2to3` (FM-2).

## Scope gate (do not over-build)

Everything traces to a spec FR. No LRO (`google.longrunning`) — deferred. No Go client, CLI,
or Terraform code — later phases. `x-aip-*` set is exactly what P1/P2 read; nothing
speculative beyond the lossless mandate.
