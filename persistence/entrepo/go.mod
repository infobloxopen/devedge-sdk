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

go 1.25.5

require (
	entgo.io/ent v0.14.6
	github.com/infobloxopen/devedge-sdk v0.60.0
	google.golang.org/grpc v1.81.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/infobloxopen/apis/proto/infoblox/field v1.0.0-alpha.4 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
