// WS-033 TEMPORARY workspace-local override of the published canonical apis field
// module (github.com/infobloxopen/apis/proto/infoblox/field). It carries the same
// module PATH so the workspace (go.work) and the cmd module (a go.mod replace)
// resolve infoblox.field.v1 to THIS binding, which is generated from the local
// mirror and includes credential (10) + credential_prefix (11). The published
// apis module has no such fields yet; when it ships them, delete this directory,
// drop the go.work entry and the cmd/go.mod replace, and re-pin the require.
module github.com/infobloxopen/apis/proto/infoblox/field

go 1.23

require google.golang.org/protobuf v1.36.11
