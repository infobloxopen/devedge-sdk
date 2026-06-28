.PHONY: build test security-check vet lint tidy generate sync-scaffold-mirrors \
        build-gowork-off check-graph-isolation

# MODULES is every Go module in this repo (WS-011 / F039 multi-module split). The
# build/vet/test targets loop over it so each module's gates run; go.work resolves
# the cross-module references locally. As adapters split out (config/koanf,
# events/kafkabus, persistence/*), append each new module dir here.
MODULES := . cmd config/koanf events/kafkabus observability/otel persistence/entrepo persistence/gormtx

# Regenerate protobuf Go bindings (the authz annotation + the authzpb test fixture)
# and the <Service>AuthzRules tables. Requires buf + protoc-gen-go on PATH; the
# devedge-authz plugin is built locally to ./bin and put on PATH for buf.
generate:
	# Engine-neutral storage options (infoblox.storage.v1.model) are CANONICAL
	# (github.com/infobloxopen/apis/proto/infoblox/storage/v1) — the Go binding comes
	# from that module (see go.mod); proto/infoblox/storage/v1/storage.proto is only a
	# buf import-resolution mirror, never generated here.
	go build -o bin/protoc-gen-devedge-authz ./cmd/protoc-gen-devedge-authz
	go build -o bin/protoc-gen-svc           ./cmd/protoc-gen-svc
	go build -o bin/protoc-gen-storage       ./cmd/protoc-gen-storage
	go build -o bin/protoc-gen-ent           ./cmd/protoc-gen-ent
	# SDK-OWNED infoblox.ddd.v1 annotation binding: generated LOCALLY (the one
	# annotation .pb.go the repo generates in-repo — safe, SDK-private namespace).
	# Run before the others so protoc-gen-{ent,svc} can import the dddv1 binding.
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.ddd.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.toy.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.apikey.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.fleet.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.iam.yaml
	cd testdata/toy && go mod tidy
	cd testdata/apikey && go mod tidy
	cd testdata/fleet && go mod tidy
	cd testdata/iam && go mod tidy
	go mod tidy
	@echo "NOTE: protoc-gen-ent regenerates ent SCHEMAS only. If a relationship/"
	@echo "      resource shape changed, regenerate the ent CLIENT too:"
	@echo "        cd testdata/apikey && go generate ./ent"
	@echo "        cd testdata/fleet  && go generate ./ent"

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

security-check: ## Run security assertions (go test -run Security)
	# testdata/* are standalone consumer modules (own go.mod + `replace` to the SDK),
	# deliberately NOT in go.work — so GOWORK=off, or the workspace rejects their dir.
	cd testdata/toy && GOWORK=off go test ./... -run Security -v

# Uses golangci-lint if installed; falls back to go vet.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not found; running go vet"; go vet ./...; fi

tidy:
	@set -e; for m in $(MODULES); do echo "== tidy $$m =="; (cd $$m && go mod tidy); done
