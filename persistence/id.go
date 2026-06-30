package persistence

import "github.com/google/uuid"

// IDGenerator mints a fresh primary-identifier string for a resource that is
// created without a caller-supplied id. It is the seam the generated repository
// adapters use to realize server-generated identity (the default): on Create,
// when the incoming id is empty, the adapter asks its IDGenerator for one before
// persisting, so an empty id is never written.
//
// The seam is intentionally tiny — a single NewID() string — so a host can swap
// the format (ULID, snowflake, a tenant-prefixed scheme) without touching the
// generated code: pass a custom IDGenerator through the generated constructor's
// functional option. NewID must return a non-empty, collision-resistant value
// and must be safe for concurrent use.
type IDGenerator interface {
	// NewID returns a fresh, non-empty identifier.
	NewID() string
}

// IDGeneratorFunc adapts a plain func() string into an IDGenerator, so a host can
// supply a one-line generator without declaring a type.
type IDGeneratorFunc func() string

// NewID calls the underlying function.
func (f IDGeneratorFunc) NewID() string { return f() }

// uuid7Generator mints time-ordered UUIDv7 identifiers. UUIDv7 is the default
// because its leading timestamp makes ids index-friendly (inserts append near the
// B-tree tail instead of scattering, as UUIDv4 does), while remaining globally
// unique. If the system clock/entropy source fails, uuid.NewV7 returns an error;
// rather than persist an empty id, NewID falls back to a random UUIDv4 so the seam
// never yields the empty string.
type uuid7Generator struct{}

// NewID returns a fresh UUIDv7 string, falling back to UUIDv4 if v7 generation
// fails (so the contract "never empty" always holds).
func (uuid7Generator) NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

// uuid4Generator mints random UUIDv4 identifiers.
type uuid4Generator struct{}

// NewID returns a fresh UUIDv4 string.
func (uuid4Generator) NewID() string { return uuid.NewString() }

// UUID7Generator returns the built-in time-ordered UUIDv7 IDGenerator. It is the
// generator the codegen wires for an id annotated GENERATOR_UUID7 (or unspecified)
// and the value of DefaultIDGenerator.
func UUID7Generator() IDGenerator { return uuid7Generator{} }

// UUID4Generator returns the built-in random UUIDv4 IDGenerator. It is the
// generator the codegen wires for an id annotated GENERATOR_UUID4.
func UUID4Generator() IDGenerator { return uuid4Generator{} }

// DefaultIDGenerator is the package-level default used when a generated
// constructor is not given an explicit IDGenerator, and the generator the codegen
// wires for an id annotated GENERATOR_CUSTOM (the host overrides it via the
// constructor option, or replaces this package variable process-wide). It is
// UUIDv7 — time-ordered, index-friendly identity by default.
var DefaultIDGenerator IDGenerator = uuid7Generator{}

// RepoConfig holds the host-tunable settings a generated repository constructor
// applies. Today it carries only the IDGenerator; it is a struct (not a bare
// field) so future per-repo options can be added without changing the generated
// constructor signatures — they already take ...RepoOption.
type RepoConfig struct {
	// IDGenerator mints server-generated ids on Create. NewRepoConfig defaults it
	// to the per-resource built-in the codegen selected; WithIDGenerator overrides.
	IDGenerator IDGenerator
}

// RepoOption configures a generated repository constructor. Options are applied
// in order over a RepoConfig seeded with the codegen-selected defaults.
type RepoOption func(*RepoConfig)

// WithIDGenerator overrides the IDGenerator a generated repository uses to mint
// server-generated ids. A nil generator is ignored (the default is kept), so a
// caller can never accidentally disable id generation.
func WithIDGenerator(g IDGenerator) RepoOption {
	return func(c *RepoConfig) {
		if g != nil {
			c.IDGenerator = g
		}
	}
}

// NewRepoConfig builds a RepoConfig seeded with the given default IDGenerator
// (the per-resource built-in the codegen selected) and applies opts over it. If
// the default is nil it falls back to DefaultIDGenerator so the IDGenerator is
// never nil. Generated constructors call this to resolve their effective config.
func NewRepoConfig(defaultGen IDGenerator, opts ...RepoOption) RepoConfig {
	if defaultGen == nil {
		defaultGen = DefaultIDGenerator
	}
	cfg := RepoConfig{IDGenerator: defaultGen}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.IDGenerator == nil {
		cfg.IDGenerator = DefaultIDGenerator
	}
	return cfg
}
