// persistence/gormtx is a NESTED Go module (WS-011 / F039): the gorm-backed
// transaction runner + outbox/idempotency/leader machinery lives here, not in
// the root module's dependency graph. The module path equals the import path, so
// no .go import statement changes — a consumer pulls gorm only when it `require`s
// THIS module.
//
// It requires the root devedge-sdk module (the adapter implements the core
// persistence.TxRunner + events seams). The local go.work at the repo root
// resolves that require to the working tree during dev/CI; the require below is
// the version a published consumer resolves, bumped by the synchronized release
// script (P3).
module github.com/infobloxopen/devedge-sdk/persistence/gormtx

go 1.25.5

require (
	github.com/infobloxopen/devedge-sdk v0.30.0
	google.golang.org/grpc v1.81.1
	gorm.io/gorm v1.31.1
	modernc.org/sqlite v1.52.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/infobloxopen/apis/proto/infoblox/field v1.0.0-alpha.1 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
