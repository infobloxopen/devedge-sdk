// cmd is a NESTED Go module (WS-011 / F039): the devedge-sdk CLI + the
// protoc-gen-* codegen plugins live here. Owner decision (2026-06-27): it is fine
// for the CLI to import ANYTHING (ent, gorm, …) — what must stay light is the
// LIBRARY that downstream apps import (the root module). The CLI imports
// entgo.io/ent/cmd/ent (entc.Generate) during scaffolding; keeping that here,
// behind a module boundary, keeps ent + gorm out of the root library's graph.
//
// Install paths are UNCHANGED: `go install
// github.com/infobloxopen/devedge-sdk/cmd/protoc-gen-svc@vX.Y.Z` resolves this
// module at its synchronized `cmd/vX.Y.Z` tag (module path == import path). The
// local go.work at the repo root resolves the root require to the working tree
// during dev/CI; the require below is the version a published install resolves,
// bumped by the synchronized release script (P3).
module github.com/infobloxopen/devedge-sdk/cmd

go 1.25.5

require (
	github.com/getkin/kin-openapi v0.140.0
	github.com/infobloxopen/apis/proto/infoblox/authz v1.0.0-alpha.4
	github.com/infobloxopen/apis/proto/infoblox/field v1.0.0-alpha.1
	github.com/infobloxopen/apis/proto/infoblox/storage v1.0.0-alpha.1
	github.com/infobloxopen/devedge-sdk v0.34.0
	github.com/spf13/cobra v1.10.2
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/go-openapi/jsonpointer v0.22.5 // indirect
	github.com/go-openapi/swag/jsonname v0.25.5 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/oasdiff/yaml v0.1.0 // indirect
	github.com/oasdiff/yaml3 v0.0.13 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.81.1 // indirect
)
