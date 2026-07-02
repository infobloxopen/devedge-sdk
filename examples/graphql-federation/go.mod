// examples/graphql-federation is a SELF-CONTAINED runnable sample of the F042
// cross-service GraphQL federation gateway (WS-021 P3). Like the testdata
// fixtures it is a standalone consumer module (own go.mod + replace to the
// working tree) and is NOT part of any released module — its tests run in CI
// under GOWORK=off.
//
// It reuses the F041 two-service fixture's generated code (testdata/federation:
// asset.v1 references region.v1, sqlite-backed, guaranteed BatchGet) so the
// sample exercises the real annotation -> metadata -> BatchGet -> gateway path
// without re-running codegen, then wires those two services + the federationgql
// gateway into one GraphQL endpoint.
module github.com/infobloxopen/devedge-sdk/examples/graphql-federation

go 1.25.5

require (
	github.com/infobloxopen/devedge-sdk v0.45.0
	github.com/infobloxopen/devedge-sdk/federationgql v0.45.0
	github.com/infobloxopen/devedge-sdk/testdata/federation v0.0.0
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/sqlite v1.53.0
)

require (
	github.com/graphql-go/graphql v0.8.1
	github.com/infobloxopen/devedge-sdk/persistence/entrepo v0.45.0 // indirect
)

require (
	ariga.io/atlas v0.36.2-0.20250730182955-2c6300d0a3e1 // indirect
	entgo.io/ent v0.14.6 // indirect
	github.com/agext/levenshtein v1.2.3 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/bmatcuk/doublestar v1.3.4 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/inflect v0.19.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/hashicorp/hcl/v2 v2.18.1 // indirect
	github.com/infobloxopen/apis/proto/infoblox/authz v1.0.0-alpha.4 // indirect
	github.com/infobloxopen/apis/proto/infoblox/field v1.0.0-alpha.3 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/zclconf/go-cty v1.14.4 // indirect
	github.com/zclconf/go-cty-yaml v1.1.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.69.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	gorm.io/gorm v1.31.1 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/infobloxopen/devedge-sdk => ../..

replace github.com/infobloxopen/devedge-sdk/federationgql => ../../federationgql

replace github.com/infobloxopen/devedge-sdk/persistence/entrepo => ../../persistence/entrepo

replace github.com/infobloxopen/devedge-sdk/testdata/federation => ../../testdata/federation
