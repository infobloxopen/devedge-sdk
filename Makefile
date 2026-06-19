.PHONY: build test security-check vet lint tidy generate

# Regenerate protobuf Go bindings (the authz annotation + the authzpb test fixture)
# and the <Service>AuthzRules tables. Requires buf + protoc-gen-go on PATH; the
# devedge-authz plugin is built locally to ./bin and put on PATH for buf.
generate:
	# Engine-neutral storage options (infoblox.storage.v1.model). Generated FIRST,
	# with protoc-gen-go only, because protoc-gen-ent imports the binding below.
	buf generate --template buf.gen.storage.yaml --path proto/infoblox/storage/v1/storage.proto
	go build -o bin/protoc-gen-devedge-authz ./cmd/protoc-gen-devedge-authz
	go build -o bin/protoc-gen-svc           ./cmd/protoc-gen-svc
	go build -o bin/protoc-gen-storage       ./cmd/protoc-gen-storage
	go build -o bin/protoc-gen-ent           ./cmd/protoc-gen-ent
	PATH="$(CURDIR)/bin:$$PATH" buf generate
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.toy.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.apikey.yaml
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.fleet.yaml
	cd testdata/toy && go mod tidy
	cd testdata/apikey && go mod tidy
	cd testdata/fleet && go mod tidy
	go mod tidy
	@echo "NOTE: protoc-gen-ent regenerates ent SCHEMAS only. If a relationship/"
	@echo "      resource shape changed, regenerate the ent CLIENT too:"
	@echo "        cd testdata/apikey && go generate ./ent"
	@echo "        cd testdata/fleet  && go generate ./ent"

build:
	go build ./...

test:
	go test ./...

security-check: ## Run security assertions (go test -run Security)
	cd testdata/toy && go test ./... -run Security -v

vet:
	go vet ./...

# Uses golangci-lint if installed; falls back to go vet.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not found; running go vet"; go vet ./...; fi

tidy:
	go mod tidy
