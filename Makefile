.PHONY: build test security-check vet lint tidy generate sync-scaffold-mirrors \
        build-gowork-off check-graph-isolation release release-verify \
        generate-migration-baseline check-migration-baseline

# MODULES is every Go module in this repo (WS-011 / F039 multi-module split). The
# build/vet/test targets loop over it so each module's gates run; go.work resolves
# the cross-module references locally. As adapters split out (config/koanf,
# events/kafkabus, persistence/*), append each new module dir here.
MODULES := . authn/oidc cmd config/koanf events/kafkabus federationgql observability/otel persistence/entrepo persistence/gormtx persistence/migrate

# Regenerate protobuf Go bindings (the authz annotation + the authzpb test fixture)
# and the <Service>AuthzRules tables. Requires buf + protoc-gen-go on PATH; the
# devedge-authz plugin is built locally to ./bin and put on PATH for buf.
generate:
	# Engine-neutral storage options (infoblox.storage.v1.model) are CANONICAL
	# (github.com/infobloxopen/apis/proto/infoblox/storage/v1) — the Go binding comes
	# from that module (see go.mod); proto/infoblox/storage/v1/storage.proto is only a
	# buf import-resolution mirror, never generated here. The same applies to
	# infoblox.field.v1 (github.com/infobloxopen/apis/proto/infoblox/field): its Go
	# binding comes from the published apis module (see go.mod), and
	# proto/infoblox/field/v1/field.proto is only a buf import-resolution mirror.
	go build -o bin/protoc-gen-devedge-authz ./cmd/protoc-gen-devedge-authz
	go build -o bin/protoc-gen-svc           ./cmd/protoc-gen-svc
	go build -o bin/protoc-gen-storage       ./cmd/protoc-gen-storage
	go build -o bin/protoc-gen-ent           ./cmd/protoc-gen-ent
	go build -o bin/openapiv2to3             ./cmd/openapiv2to3
	# SDK-OWNED infoblox.ddd.v1 annotation binding: generated LOCALLY (the one
	# annotation .pb.go the repo generates in-repo — safe, SDK-private namespace).
	# Run before the others so protoc-gen-{ent,svc} can import the dddv1 binding.
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.ddd.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.toy.yaml
	# WS-024 Part B: build the toy FileDescriptorSet so the OpenAPI enrichment pass
	# can recover the full AIP contract (field_behavior, resource identity, method
	# classification, references, pagination) from proto — the lossless interchange.
	buf build testdata/toy -o testdata/toy/toy.binpb
	# WS-013 EMIT + WS-024 ENRICH: convert the toy v2 swagger to OpenAPI v3 and run
	# the proto-authoritative enrichment pass (lands in testdata/toy/openapi/).
	bin/openapiv2to3 -descriptor testdata/toy/toy.binpb testdata/toy/toy.swagger.json testdata/toy/openapi
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.apikey.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.fleet.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.iam.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.federation.yaml
	cd testdata/toy && go mod tidy
	cd testdata/apikey && go mod tidy
	cd testdata/fleet && go mod tidy
	cd testdata/iam && go mod tidy
	cd testdata/federation && go mod tidy
	go mod tidy
	@echo "NOTE: protoc-gen-ent regenerates ent SCHEMAS only. If a relationship/"
	@echo "      resource shape changed, regenerate the ent CLIENT too:"
	@echo "        cd testdata/apikey     && go generate ./ent"
	@echo "        cd testdata/fleet      && go generate ./ent"
	@echo "        cd testdata/federation && go generate ./ent"

# Refresh the annotation .proto mirrors embedded in the `devedge-sdk new service`
# scaffold (cmd/devedge-sdk) from the canonical proto/infoblox source. The
# scaffold vendors these into generated projects so buf can resolve the
# infoblox/{authz,field} imports. TestMirrorsMatchSDKSource fails if they drift.
sync-scaffold-mirrors:
	cp proto/infoblox/authz/v1/authz.proto cmd/devedge-sdk/internal/scaffold/mirrors/infoblox/authz/v1/authz.proto
	cp proto/infoblox/field/v1/field.proto cmd/devedge-sdk/internal/scaffold/mirrors/infoblox/field/v1/field.proto
	cp proto/infoblox/storage/v1/storage.proto cmd/devedge-sdk/internal/scaffold/mirrors/infoblox/storage/v1/storage.proto
	cp proto/infoblox/ddd/v1/ddd.proto cmd/devedge-sdk/internal/scaffold/mirrors/infoblox/ddd/v1/ddd.proto

# build/vet/test loop over every module (go.work resolves cross-module refs).
build:
	@set -e; for m in $(MODULES); do echo "== build $$m =="; (cd $$m && go build ./...); done

test:
	@set -e; for m in $(MODULES); do echo "== test $$m =="; (cd $$m && go test ./...); done

vet:
	@set -e; for m in $(MODULES); do echo "== vet $$m =="; (cd $$m && go vet ./...); done

# build-gowork-off builds each module with the workspace DISABLED, so a published
# module that is missing a `require` (which go.work would silently satisfy from the
# working tree) fails here instead of after release. The go.work-masking failure
# mode the spec names is caught by this gate.
#
# The root builds with a plain in-place GOWORK=off (real requires only — the
# load-bearing check that the SHED root still resolves). A nested adapter requires
# the root at a PUBLISHED version (v0.26.1) that pre-dates the carve-out and still
# contains the adapter's subpackage; with GOWORK off that root require overlaps the
# module's own path ("provided by exactly one module"). To validate the adapter's
# OWN requires today (pre-release) without that overlap — and WITHOUT ever mutating
# the committed go.mod — we copy the whole repo to a temp dir, point the copied
# adapter's root require at the copied root tree (a replace confined to the throw-
# away copy), and GOWORK=off-build it there. go.work owns local resolution in the
# real tree; the P3 release script bumps the require to the shed root, after which
# this copy-and-replace step becomes a no-op and can be simplified to in-place.
build-gowork-off:
	@set -e; \
	echo "== build . (GOWORK=off) =="; \
	GOWORK=off go build ./...; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	cp -R . "$$tmp/repo"; rm -f "$$tmp/repo/go.work" "$$tmp/repo/go.work.sum"; \
	for m in $(MODULES); do \
	  [ "$$m" = "." ] && continue; \
	  echo "== build $$m (GOWORK=off) =="; \
	  ( cd "$$tmp/repo/$$m" && \
	    go mod edit -replace github.com/infobloxopen/devedge-sdk="$$tmp/repo" && \
	    GOWORK=off go build ./... ); \
	done

# check-graph-isolation proves the graph-level dependency isolation (AC-1/AC-2):
# a server-only consumer's module graph is free of the heavy adapter deps, and the
# adapter is pulled in only on opt-in. Network-touching (creates a temp consumer +
# go mod tidy), so it is a dedicated CI step, not part of the hermetic `test`.
check-graph-isolation:
	./scripts/check-graph-isolation.sh

# generate-migration-baseline (re)generates the SDK framework migration baseline
# (persistence/migrate/baseline/0001_framework_init.{up,down}.sql) from the canonical
# gormtx framework models via Atlas — BUILD-TIME only (Atlas + its gorm provider live in
# the isolated schemagen tool module, never in a runtime graph). Requires the ariga.io/
# atlas CLI + Docker; see scripts/migration-baseline.sh.
generate-migration-baseline:
	./scripts/migration-baseline.sh generate

# check-migration-baseline is the native drift gate: it regenerates the baseline from the
# models and fails if the committed baseline is stale (models changed, 0001 did not). It
# SKIPS cleanly when Atlas/Docker are absent; CI installs Atlas and runs it for real.
check-migration-baseline:
	./scripts/migration-baseline.sh check

security-check: ## Run security assertions (go test -run Security)
	# testdata/* are standalone consumer modules (own go.mod + `replace` to the SDK),
	# deliberately NOT in go.work — so GOWORK=off, or the workspace rejects their dir.
	cd testdata/toy && GOWORK=off go test ./... -run Security -v

# Uses golangci-lint if installed; falls back to go vet.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not found; running go vet"; go vet ./...; fi

tidy:
	@set -e; for m in $(MODULES); do echo "== tidy $$m =="; (cd $$m && go mod tidy); done

# release cuts the SYNCHRONIZED multi-module release (WS-011 / F039) with TAG-ROOT-
# FIRST ordering: it bumps the scaffold version source + each adapter's
# `require github.com/infobloxopen/devedge-sdk` to VERSION, then phase 1 commits +
# tags + PUSHES the root tag; phase 2 finalizes each adapter's go.sum against that
# pushed root tag (the real root@VERSION checksum — a filesystem replace would leave
# it absent and break `-mod=readonly` builds), commits, and tags the adapters. NEVER
# `go work sync` (it empties member requires in this nested layout). Default is a DRY
# RUN that prints the two-phase plan + stages the edits; pass PUSH=1 for the real run.
#   make release VERSION=v0.27.0              # dry run (no commit/tag/push)
#   make release VERSION=v0.27.0 VALIDATE=1   # local go.sum smoke (no real tag/network)
#   make release VERSION=v0.27.0 PUSH=1       # the real two-phase release
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z [PUSH=1|VALIDATE=1]"; exit 2; }
	./scripts/release.sh $(VERSION) $(if $(PUSH),--push,)$(if $(VALIDATE),--validate,)

# release-verify confirms an EXTERNAL consumer resolves every module at VERSION after
# a push. Uses the explicit @VERSION (not @latest — the proxy's @latest view lags a
# few minutes behind a fresh tag); GONOSUMCHECK is not needed (public modules).
release-verify:
	@test -n "$(VERSION)" || { echo "usage: make release-verify VERSION=vX.Y.Z"; exit 2; }
	@set -e; tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	cd "$$tmp" && go mod init release-verify >/dev/null 2>&1; \
	for m in $(MODULES); do \
	  path="github.com/infobloxopen/devedge-sdk"; [ "$$m" = "." ] || path="$$path/$$m"; \
	  echo "== go get $$path@$(VERSION) =="; \
	  GOFLAGS=-mod=mod go get "$$path@$(VERSION)"; \
	done; \
	echo "all modules resolved at $(VERSION)"
