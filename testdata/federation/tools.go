//go:build tools

// Package tools pins the ent code generator (entc) as a module dependency so
// `go mod tidy` keeps its requirements in go.sum. Without this, tidy prunes the
// entc-only deps (cobra, inflect, atlas, ...) because no ordinary package
// imports them, and the next `go generate ./ent` fails with "missing go.sum
// entry". The `tools` build tag keeps this file out of normal builds.
package tools

import _ "entgo.io/ent/cmd/ent"
