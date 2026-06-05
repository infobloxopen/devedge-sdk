.PHONY: build test vet lint tidy generate

# Regenerate protobuf Go bindings (the authz annotation + the authzpb test fixture)
# and the <Service>AuthzRules tables. Requires buf + protoc-gen-go on PATH; the
# devedge-authz plugin is built locally to ./bin and put on PATH for buf.
generate:
	go build -o bin/protoc-gen-devedge-authz ./cmd/protoc-gen-devedge-authz
	PATH="$(CURDIR)/bin:$$PATH" buf generate
	go mod tidy

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

# Uses golangci-lint if installed; falls back to go vet.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not found; running go vet"; go vet ./...; fi

tidy:
	go mod tidy
