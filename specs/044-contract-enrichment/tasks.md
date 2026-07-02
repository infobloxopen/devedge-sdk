# Tasks: 044 contract enrichment

Model routing: `[S]` mechanical/localized (Sonnet-suitable), `[C]` cross-cutting/design
(Opus). Gate: every task `[X]`, tests green, golden clean, e2e redaction observed.

## Part A — field_behavior contract

- [X] **T044-1 [C]** Create `internal/aip` package. Add `FieldBehavior` alias/type over
  `google.golang.org/genproto/googleapis/api/annotations.FieldBehavior` and
  `ResolveFieldBehavior(fd protoreflect.FieldDescriptor) ([]FieldBehavior, error)`: union of
  explicit `field_behavior` + derived from `fieldv1.E_Opts` (secret→INPUT_ONLY;
  id.strategy SERVER_GENERATED→OUTPUT_ONLY / USER_SETTABLE→IMMUTABLE); NEVER not_null→REQUIRED;
  allowed_values exposed separately (→enum). Fail-loud on contradictions (OUTPUT_ONLY+REQUIRED,
  OUTPUT_ONLY+INPUT_ONLY, explicit-vs-derived), error names msg/field/behaviors. Unit tests.
- [X] **T044-2 [C]** Move `classifyMethod`, `detectServiceResource`, and resource-pattern/AIP-122
  identity logic from `cmd/protoc-gen-svc/main.go` into `internal/aip`, rewritten to take
  `protoreflect.MethodDescriptor`/`MessageDescriptor`. Export `ClassifyMethod`, `StdMethod`,
  `ResourceIdentity{Type,Patterns,Key}`. svc plugin calls them with `.Desc`. Parity test on toy.
- [X] **T044-3 [C]** Re-point the three `== OUTPUT_ONLY` read sites to `aip.ResolveFieldBehavior`:
  `protoc-gen-svc/main.go:200-215`, `protoc-gen-storage/main.go:172-179`,
  `protoc-gen-ent/main.go:176-183`. Preserve existing OUTPUT_ONLY behavior (soft-delete,
  column omission, ent output-only) — golden-diff must be unchanged for those.
- [X] **T044-4 [S]** `middleware/redact`: strip `INPUT_ONLY` fields from responses (superset of
  the secret case). Keep secret's storage encryption path unchanged. Unit + reflection test.
- [X] **T044-5 [S]** Add `REQUIRED`, `IMMUTABLE`, `INPUT_ONLY` example fields to fixtures
  (`testdata/toy/widgets.proto`, `testdata/apikey/apikey.proto`, `testdata/iam/iam.proto`) and to
  the scaffold template `cmd/devedge-sdk/internal/scaffold/templates/proto.proto.tmpl`. Include a
  USER_SETTABLE id (derives IMMUTABLE) and a not_null-but-not-required field (proves FM-4).

## Part B — lossless enriched OpenAPI

- [X] **T044-6 [S]** Add FDS producer to `make generate`: `buf build -o testdata/toy/toy.binpb`
  (Makefile + any buf config), ordered before `openapiv2to3`. Ensure `.binpb` is a tracked
  golden or a regenerable intermediate (match repo convention; check-in the toy one for the test).
- [X] **T044-7 [C]** Extend `cmd/openapiv2to3/main.go`: accept the FDS path (`--descriptor` flag
  or 3rd arg), unmarshal `descriptorpb.FileDescriptorSet` → `protodesc.NewFiles`, and run an
  enrichment pass on the in-memory `openapi3.T` between `ToV3` (~L46) and serialization (~L49)
  using `internal/aip`. Write: native `readOnly`(OUTPUT_ONLY)/`writeOnly`(INPUT_ONLY)/`enum`
  (allowed_values) + schema `required`(REQUIRED); `x-aip-field-behavior` (raw list, carries
  IMMUTABLE); `x-aip-resource`(type/pattern/key); `x-aip-method`(per op); `x-aip-pagination`(List);
  `x-aip-references`(WS-021 resource_reference). Fail-loud on missing FDS / FDS-swagger drift.
- [X] **T044-8 [S]** Regenerate + check in golden `testdata/toy/openapi/toy.openapi.yaml`.
  Assert (test) native fields + every `x-aip-*` on toy resources.

## Cross-cutting / QA

- [X] **T044-9 [C]** Drift golden (AC-6): a test feeds the toy FDS to both the generator
  classification and the enrichment classification and asserts identical results.
- [X] **T044-10 [S]** Failure fixtures: contradictory-annotation proto aborts codegen with named
  error (FM-1); missing FDS aborts `openapiv2to3` (FM-2). Table/exec tests.
- [X] **T044-11 [C]** e2e: extend the toy server redaction test to issue a real REST
  create+get and assert the INPUT_ONLY/secret field is absent from the wire response (runtime
  boundary, not codegen-only).
- [X] **T044-12 [S]** `go build ./... && make test && make generate && git diff --exit-code`
  clean; `scripts/check-graph-isolation.sh` green; `make sync-scaffold-mirrors` clean; GOWORK=off
  build green. Update CHANGELOG + any docs mentioning field_behavior/OpenAPI.
