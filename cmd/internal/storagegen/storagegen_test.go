package storagegen

import "testing"

// TestMappable_classification is the single source-of-truth table for the
// auto-wire-vs-fail decision both plugins consume (F027 G-005 parity). If a
// kind's verdict changes, it changes for ent and GORM together.
func TestMappable_classification(t *testing.T) {
	cases := []struct {
		name string
		f    Field
		want bool
	}{
		{"id", Field{Name: "id", IsID: true}, true},
		{"tenant", Field{Name: "account_id", IsTenant: true}, true},
		{"secret", Field{Name: "key_value", IsSecret: true}, true},
		{"tags", Field{Name: "tags", IsTags: true}, true},
		{"plain scalar", Field{Name: "vin", HasColumnType: true}, true},
		{"output-only scalar (name)", Field{Name: "name", OutputOnly: true}, true},
		{"scalar FK", Field{Name: "fleet_id", IsScalarFK: true, HasColumnType: true}, true},
		{"relationship message", Field{Name: "fleet", IsMessage: true, IsRelationship: true}, true},
		{"relationship repeated", Field{Name: "vehicles", IsRepeated: true, IsRelationship: true}, true},
		// Unmappable — must fail generation, not be silently dropped:
		{"nested non-relationship message", Field{Name: "spec", IsMessage: true}, false},
		{"repeated non-relationship", Field{Name: "aliases", IsRepeated: true}, false},
		{"enum", Field{Name: "state", IsEnum: true}, false},
		{"non-string map (message)", Field{Name: "counts", IsMessage: true}, false},
		{"unknown scalar (no column type)", Field{Name: "weird"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Mappable(c.f); got != c.want {
				t.Errorf("Mappable(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestClassify_partitions(t *testing.T) {
	fields := []Field{
		{Name: "id", IsID: true},
		{Name: "vin", HasColumnType: true},
		{Name: "spec", IsMessage: true},     // unmapped
		{Name: "aliases", IsRepeated: true}, // unmapped
	}
	auto, unmapped := Classify(fields)
	if len(auto) != 2 {
		t.Errorf("auto = %d, want 2", len(auto))
	}
	if len(unmapped) != 2 {
		t.Fatalf("unmapped = %d, want 2", len(unmapped))
	}
	if Reason(unmapped[0]) == "" || Reason(unmapped[1]) == "" {
		t.Error("expected a non-empty Reason for each unmapped field")
	}
}
