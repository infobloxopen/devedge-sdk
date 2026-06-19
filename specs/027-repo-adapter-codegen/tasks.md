# F027 Tasks — Repository Adapter Codegen

Model routing: `[S]` = Sonnet subagent (mechanical), `[C]` = Opus (generator/design logic).
Gate: each phase's tests green before the next. No back-compat — clean implementation.

## Phase 1 — ent adapter generator (pure, green unit increment) ✅ done
- [X] T101 `[C]` Add `renderEntRepoAdapter(msg, pkgName, goImportPath)` to `cmd/protoc-gen-ent/render.go`: emits `New<R>EntRepository` filling the six `entrepo.EntRepository` closures (Create/Get/List/Update/Delete/Undelete) + `fromEnt<R>` + a `LookupBy<Secret>Hash` helper per secret field, reusing the existing `writable`/`secrets` partition, `entGoName`/`entSetterGoName`, tenant + soft-delete mutation guards, `ConstraintError`, filter/paging from the generated `<R>EntColumns` maps. FR G-001
- [X] T102 `[C]` Render tests in `render_repo_test.go`: a tenant+secret+soft-delete+TTL+etag+tags shape and a minimal no-tenant/no-secret/hard-delete shape, each gated by `go/format.Source` (real syntax check) + substring assertions (closures, secret hash/cipher + cleared plaintext, etag line, tenant/soft-delete/undelete predicates, `ConstraintError`, `FilterPredicate`, `LookupBy…Hash`, secret-not-projected). AC-002
- [ ] T103 `[S]` Golden-diff vs `apikey`'s hand-written `ent_wiring.go` — **folded into Phase 2** (the true equivalence test is: delete the hand-written file, regenerate, build+test green). Deferred to T202/T204.
- [X] T104 `[S]` `go test ./cmd/protoc-gen-ent/...` green; `go build ./cmd/...` + `go vet` clean (no `main.go`/fixture changes yet — zero blast radius).

## Phase 2 — wire into main.go + migrate fixtures + runtime tests ✅ done
- [X] T201 `[C]` Emit `<snake>_repo.ent.go` from `main.go` (per-message loop, after the columns loop).
- [X] T202 `[S]` Deleted hand-written `ent_wiring.go` from `testdata/apikey` + `testdata/fleet`; `make generate` emits `api_key_repo.ent.go`, `fleet_repo.ent.go`, `vehicle_repo.ent.go`; no symbol collisions. AC-001. **Surfaced + fixed two latent drifts the hand-written files had vs the proto:** Fleet has `delete_time` → generated adapter now does soft-delete + `Undelete_` (hand-written hard-deleted); apikey `name` is OUTPUT_ONLY → surfaced on read but never written (hand-written wrote it). Also added FK-guard (`if entity.GetFleetId() != ""`) for belongs_to scalar FKs.
- [X] T203 `[C]` Runtime validation (AC-002) via the **existing comprehensive ent suites** now running against the generated adapter — apikey: `ent_repository`, `ent_tenant_mutation`, `security_isolation`, `softdelete_sqlite`, `softdelete_unique_sqlite`, `etag_ttl_sqlite`, `tags_sqlite`, `update_zerovalue_sqlite`, `batch_conformance`, `sqlite`; fleet: `ent_relationship`. All green. (No new test file needed — the fixtures already cover tenant scope, soft-delete+undelete, ConstraintError→409/412, secret-at-rest, AIP-160 filter, etag round-trip.)
- [X] T103 `[S]` Golden equivalence (deferred from Phase 1): met — the generated adapter passes every test the hand-written one did, and is *more* correct (the two drift fixes above).
- [X] T204 `[S]` Green: root `go build`/`go vet`/`go test ./...`, `testdata/{apikey,fleet,toy}` `go test ./...`, `make security-check`. (`make generate` also tidied `testdata/apikey/go.mod` — pruned stale indirect deps; expected, it's part of the target.)

## Phase 3 — shared fail-closed field-coverage checker ✅ done
- [X] T301 `[C]` New engine-neutral `cmd/internal/storagegen` package: `Field`, `Mappable`, `Classify(fields) (auto, unmapped)`, `Reason(f)` — the single source of the auto-wire-vs-fail decision, derived from proto annotations only (no ent/GORM types), so it's reusable by `protoc-gen-storage` in Phase 6. FR G-002/G-005
- [X] T302 `[C]` `protoc-gen-ent` consumes it: added `IsEnum` to field info, a `toStorageFields` bridge, and a per-message check in `main.go` that calls `gen.Error("protoc-gen-ent: <Msg>.<field>: <reason>")` and aborts generation when any field is unmapped (nested non-relationship message, repeated non-relationship, enum, non-string map). AC-003. `make generate` confirms **no false positives** on the real fixtures.
- [X] T303 `[S]` Unit tests: `storagegen` classification table (the parity source of truth) + a plugin-level bridge test (unmappable fields flagged; belongs_to message + scalar FK not flagged). AC-005

> Note: the `gen.Error` path is the canonical protogen failure mechanism (recorded error → CodeGeneratorResponse error → buf fails non-zero). The classification + bridge are unit-tested; a full e2e "buf generate fails on a bad proto" fixture is a nice-to-have follow-up.

## Phase 4 — owned override seam ✅ done
- [X] T401 `[C]` The generated adapter declares exported nil hook vars and calls them when set: `FromEnt<R>Custom(e, p)` at the end of `fromEnt<R>` (computed/derived read fields), and `ToEnt<R>OnCreate(p, b)` / `ToEnt<R>OnUpdate(p, u)` just before the ent builder saves (custom write columns). FR G-003.
- [X] T402 `[C]` **Obviated — no scaffolder/CLI needed.** The package-var hook idiom means the developer registers hooks from their OWN regen-safe file (e.g. an `init()`); there is no generated file to scaffold-once or clobber. This is simpler than a `_projection_custom.go` generator and still satisfies the split-files "owned, survives regen" goal. (Fail-closed (Phase 3), not a hook, handles unmapped fields — so no `// devedge:wire` stubs are needed.)
- [X] T403 `[S]` Runtime test `testdata/apikey/.../repo_custom_hook_test.go`: registers the exported hooks from the external test package, proves the read hook runs after the deterministic projection (Create + Get) and the create hook runs before save; render test asserts the hook vars + call sites are emitted. Regeneration is idempotent — hooks live in the generated file; registration lives in the consumer's file, untouched by codegen.

## Phase 5 — neutral multi-surface annotation
- [ ] T501 `[C]` Define `proto/infoblox/storage/v1/storage.proto` MessageOptions extension `model`; generate Go; vendor/wire into the plugins. FR G-004
- [ ] T502 `[C]` Group messages by resolved model name → one repo + N projections; conflicting field types across surfaces → fail-closed. AC-004
- [ ] T503 `[S]` Multi-surface fixture + round-trip test. AC-004

## Phase 6 — GORM parity (`protoc-gen-storage`)
- [ ] T601 `[C]` `protoc-gen-storage` adopts the shared checker (T301) + the split-file + owned-hook contract + the neutral annotation. FR G-005
- [ ] T602 `[S]` Regenerate toy/apikey/fleet GORM; `go test ./cmd/protoc-gen-storage/...` + integration green. AC-006

## Phase 7 — docs + cross-backend validation
- [ ] T701 `[S]` Update `reference/codegen.md` + `guides/model-a-resource.md`: the adapter is generated; the owned hook; the multi-surface annotation; remove "hand-write ent wiring" guidance.
- [ ] T702 `[C]` Re-run the Run 9 `coupond` build docs-only on ent **and** GORM with zero hand-written adapter; record the before/after churn. AC-007
- [ ] T703 `[S]` Full SDK gates + `make security-check`; close #53 in the PR.
