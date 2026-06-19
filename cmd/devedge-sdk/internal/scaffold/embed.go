package scaffold

import "embed"

// templatesFS holds the text/template sources rendered into the project tree.
//
//go:embed templates/*.tmpl
var templatesFS embed.FS

// mirrorsFS holds byte-identical copies of the infoblox/{authz,field}/v1
// annotation .proto files from this SDK module's proto/infoblox dir. apx overlays
// do not provide importable .proto, so buf still needs these mirrors to resolve
// the (infoblox.authz.v1.rule) / (infoblox.field.v1.opts) imports (D-4). They are
// pinned to the SDK version this binary was built from and copied into the
// scaffold's proto/infoblox tree. A test asserts they stay byte-identical to the
// SDK source (mirror-drift guard).
//
//go:embed mirrors/infoblox
var mirrorsFS embed.FS
