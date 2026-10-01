// persistence/entrepo is a NESTED Go module (WS-011 / F039): the ent-backed
// persistence.Repository adapter (+ filter translation, the etag/softdelete
// mixins) lives here, not in the root module's dependency graph. The module path
// equals the import path, so no .go import statement changes — a consumer pulls
// ent only when it `require`s THIS module.
//
// It requires the root devedge-sdk module (the adapter implements the core
// persistence.Repository seam over middleware/etag/secret). The local go.work at
// the repo root resolves that require to the working tree during dev/CI; the
// require below is the version a published consumer resolves, bumped by the
// synchronized release script (P3).
module github.com/infobloxopen/devedge-sdk/persistence/entrepo

go 1.26.0

require (
	entgo.io/ent v0.14.6
	github.com/infobloxopen/devedge-sdk v0.64.0
	google.golang.org/grpc v1.84.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/infobloxopen/apis/proto/infoblox/field v1.0.0-alpha.5 // indirect
	github.com/infobloxopen/apis/proto/infoblox/storage v1.0.0-alpha.2 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260921155816-b14227669459 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260918162117-cecb64721679 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
