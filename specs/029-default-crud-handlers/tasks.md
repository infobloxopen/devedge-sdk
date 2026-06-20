# F029 — Tasks (Plan)

Each task tagged `[S]` (mechanical) or `[C]` (complex). Implement top-to-bottom.

## Phase 1 — protoc-gen-svc: standard-method detection + handler render

- T-101 `[C]` In `cmd/protoc-gen-svc/main.go`, enrich extraction: per service resolve the
  resource message it operates on (from Create/Get/List/Update request+response field shapes),
  whether that resource is soft-delete (resource msg has OUTPUT_ONLY `delete_time` Timestamp),
  and whether the resource carries an AIP-122 `name` field / resource pattern (drives `Parse<R>Name`).
  Per method capture the request field set (names + whether a field's message type == resource;
  presence of `update_mask`, `page_size`, `page_token`, `id`, `name`, repeated-resource in resp +
  `next_page_token`). Build a `stdMethod` classification (create/get/list/update/delete/undelete/none).
- T-102 `[C]` In `render.go`, classify each method by request/response shape (D-2): Create, Get,
  Delete (id OR name → ParseName), List (paging + optional filter/order_by/show_deleted), Update
  (resource + update_mask), Undelete (soft-delete only). Tolerate extra optional fields. No match → leave
  to Unimplemented embed. Batch (AIP-137): best-effort delegate when clean, else Unimplemented.
- T-103 `[C]` Render `type <Svc>CRUDHandler struct { Unimplemented<Svc>Server; Repo persistence.Repository[*<R>, string] }`
  (DO NOT EDIT) with one method per detected standard RPC delegating to `Repo`. read_mask/field_mask
  interceptor-applied (don't touch); tenant-stamp is the repo's job (don't duplicate).
- T-104 `[S]` Render `New<Svc>Handler(repo) *<Svc>CRUDHandler` and
  `Register<Svc>WithRepository(s *server.Server, repo) error` (= New + Register<Svc> + AddRules).
- T-105 `[S]` Update `Register<Svc>` to call `s.AddRules(<Svc>AuthzRules...)` and record its method
  names on the server; MOVE the boot gate out of Register (now at Serve).

## Phase 2 — server: AddRules + gate-at-Serve

- T-201 `[C]` `server/server.go`: add `AddRules(...authz.MethodRule)`, accumulate rules + registered
  method names; authz interceptor + verbMap read the FINAL accumulated set (rule-source closure);
  run `grpcauthz.AssertMethodsDeclared` over accumulated rules + registered methods at `Serve()`.
  `Config.Rules` becomes optional additive seed. Add `RecordMethods(...string)`.

## Phase 3 — protoc-gen-devedge-authz: AllAuthzRules aggregate

- T-301 `[S]` After per-service slices, emit `var AllAuthzRules = slices.Concat(<A>Rules, <B>Rules...)`
  (Go 1.23) covering every annotated service in the file.

## Phase 4 — clean rewrite (no back-compat)

- T-401 `[S]` Scaffold `main.go.tmpl` + `main.ent.go.tmpl`: drop hand-written handler + Config.Rules;
  use `Register<Svc>WithRepository(s, repo)`.
- T-402 `[S]` Regenerate fixtures (`make generate`); rewrite toy `server_test.go` to use the generated
  handler via embedding (toy keeps custom/batch/LRO methods → proves escape hatch) + `…WithRepository`.
- T-403 `[S]` apikey: verify generated CRUD handler over the APIKeyService; multi-surface via APIKeySummary.

## Phase 5 — tests + docs

- T-501 `[C]` `render_test.go`: generated handler/New/WithRepository valid Go (`go/format.Source`) +
  substring (each std method delegates; id+name detection; custom RPC Unimplemented).
- T-502 `[S]` devedge-authz render: `AllAuthzRules` test.
- T-503 `[C]` server tests: `AddRules` + gate-at-Serve (undeclared RPC → Serve errors).
- T-504 `[C]` toy runtime: CRUD via generated handler + override-via-embedding test.
- T-505 `[S]` docs: `reference/codegen.md` (+ server.md/guides) for default handler / New / WithRepository /
  embedding override / auto-wired rules.

## Verify

- `go build ./... && go vet ./...`; `make generate` (checkout `*_grpc.pb.go` tool drift);
  `go test ./...` root + apikey + fleet + toy; `make security-check`; scaffold e2e; fail-closed at Serve.
</content>
</invoke>
