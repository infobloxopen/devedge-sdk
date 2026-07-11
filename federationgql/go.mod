// Package federationgql lives in its OWN module so the GraphQL runtime library
// (github.com/graphql-go/graphql) stays a TRANSITIVE dependency, never part of a
// server-only consumer's module graph (the repo's check-graph-isolation gate,
// F042 AC-6 / WS-011 pattern). It requires the root SDK for the reference seam it
// composes over; the release tags it at the synchronized version alongside the
// other nested modules.
module github.com/infobloxopen/devedge-sdk/federationgql

go 1.25.5

require (
	github.com/graphql-go/graphql v0.8.1
	github.com/infobloxopen/devedge-sdk v0.63.0
)
