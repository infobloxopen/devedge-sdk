package persistence_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// TestUUID7Generator_ReturnsV7 confirms the default built-in mints valid,
// non-empty, version-7 (time-ordered) UUIDs.
func TestUUID7Generator_ReturnsV7(t *testing.T) {
	g := persistence.UUID7Generator()
	id := g.NewID()
	if id == "" {
		t.Fatal("NewID returned empty")
	}
	u, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("NewID = %q, not a valid UUID: %v", id, err)
	}
	if u.Version() != 7 {
		t.Errorf("NewID version = %d, want 7", u.Version())
	}
}

// TestUUID4Generator_ReturnsV4 confirms the uuid4 built-in mints version-4 UUIDs.
func TestUUID4Generator_ReturnsV4(t *testing.T) {
	id := persistence.UUID4Generator().NewID()
	u, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("NewID = %q, not a valid UUID: %v", id, err)
	}
	if u.Version() != 4 {
		t.Errorf("NewID version = %d, want 4", u.Version())
	}
}

// TestGenerators_AreUnique confirms successive ids differ (collision-resistant).
func TestGenerators_AreUnique(t *testing.T) {
	for _, g := range []persistence.IDGenerator{persistence.UUID7Generator(), persistence.UUID4Generator()} {
		seen := make(map[string]bool, 1000)
		for i := 0; i < 1000; i++ {
			id := g.NewID()
			if seen[id] {
				t.Fatalf("duplicate id %q from %T", id, g)
			}
			seen[id] = true
		}
	}
}

// TestDefaultIDGenerator_IsUUID7 confirms the package default is UUIDv7 (BC-12:
// time-ordered, index-friendly identity by default).
func TestDefaultIDGenerator_IsUUID7(t *testing.T) {
	u, err := uuid.Parse(persistence.DefaultIDGenerator.NewID())
	if err != nil {
		t.Fatalf("DefaultIDGenerator produced an invalid UUID: %v", err)
	}
	if u.Version() != 7 {
		t.Errorf("DefaultIDGenerator version = %d, want 7", u.Version())
	}
}

// TestIDGeneratorFunc adapts a plain func into an IDGenerator.
func TestIDGeneratorFunc(t *testing.T) {
	var g persistence.IDGenerator = persistence.IDGeneratorFunc(func() string { return "fixed" })
	if got := g.NewID(); got != "fixed" {
		t.Errorf("IDGeneratorFunc NewID = %q, want fixed", got)
	}
}

// TestNewRepoConfig_DefaultsAndOverride covers the constructor-config resolution the
// generated repositories rely on: the seeded default is used when no option is
// given; WithIDGenerator overrides it; a nil default or a nil override falls back to
// DefaultIDGenerator so the IDGenerator is never nil.
func TestNewRepoConfig_DefaultsAndOverride(t *testing.T) {
	def := persistence.UUID4Generator()

	// No options: the seeded default is kept.
	if cfg := persistence.NewRepoConfig(def); cfg.IDGenerator != def {
		t.Error("NewRepoConfig without options did not keep the seeded default")
	}

	// WithIDGenerator overrides the default.
	custom := persistence.IDGeneratorFunc(func() string { return "x" })
	if cfg := persistence.NewRepoConfig(def, persistence.WithIDGenerator(custom)); cfg.IDGenerator.NewID() != "x" {
		t.Error("WithIDGenerator did not override the default")
	}

	// A nil override is ignored (the default survives) — id generation can never be
	// accidentally disabled.
	if cfg := persistence.NewRepoConfig(def, persistence.WithIDGenerator(nil)); cfg.IDGenerator != def {
		t.Error("WithIDGenerator(nil) must keep the default")
	}

	// A nil default falls back to DefaultIDGenerator (never nil).
	if cfg := persistence.NewRepoConfig(nil); cfg.IDGenerator == nil {
		t.Error("NewRepoConfig(nil) left a nil IDGenerator")
	}
}
