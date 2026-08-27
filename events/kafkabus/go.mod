// events/kafkabus is a NESTED Go module (WS-011 / F039): the franz-go Kafka
// adapter lives here, not in the root module's dependency graph. The module
// path equals the import path, so no .go import statement changes — a consumer
// pulls franz-go only when it `require`s THIS module.
//
// It requires the root devedge-sdk module (the adapter implements the core
// events.Bus seam). The local go.work at the repo root resolves that require
// to the working tree during dev/CI; the require below is the version a
// published consumer resolves, bumped by the synchronized release script (P3).
module github.com/infobloxopen/devedge-sdk/events/kafkabus

go 1.25.5

require (
	github.com/infobloxopen/devedge-sdk v0.64.0
	github.com/twmb/franz-go v1.21.6
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/infobloxopen/apis/proto/infoblox/field v1.0.0-alpha.5 // indirect
	github.com/infobloxopen/apis/proto/infoblox/storage v1.0.0-alpha.2 // indirect
	github.com/klauspost/compress v1.18.7 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.13.1 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/grpc v1.83.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
