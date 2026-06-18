package types_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/infobloxopen/devedge-sdk/types"
)

// TestTags_Value_EmptyIsNull verifies that a nil or empty Tags persists as SQL
// NULL (a nil driver.Value), so empty maps don't write "{}".
func TestTags_Value_EmptyIsNull(t *testing.T) {
	for name, tags := range map[string]types.Tags{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			v, err := tags.Value()
			if err != nil {
				t.Fatalf("Value() error: %v", err)
			}
			if v != nil {
				t.Errorf("Value() = %v, want nil", v)
			}
		})
	}
}

// TestTags_Value_MarshalsJSONObject verifies a non-empty Tags marshals to a
// JSON object payload.
func TestTags_Value_MarshalsJSONObject(t *testing.T) {
	tags := types.Tags{"env": "prod"}
	v, err := tags.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	b, ok := v.([]byte)
	if !ok {
		t.Fatalf("Value() = %T, want []byte", v)
	}
	if got := string(b); got != `{"env":"prod"}` {
		t.Errorf("Value() = %q, want %q", got, `{"env":"prod"}`)
	}
}

// TestTags_RoundTrip verifies Value -> Scan reproduces the original map, with
// empty/nil collapsing to nil.
func TestTags_RoundTrip(t *testing.T) {
	cases := map[string]struct {
		in   types.Tags
		want types.Tags
	}{
		"nil":    {in: nil, want: nil},
		"empty":  {in: types.Tags{}, want: nil},
		"single": {in: types.Tags{"a": "1"}, want: types.Tags{"a": "1"}},
		"multi":  {in: types.Tags{"a": "1", "b": "2", "c": "3"}, want: types.Tags{"a": "1", "b": "2", "c": "3"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := tc.in.Value()
			if err != nil {
				t.Fatalf("Value() error: %v", err)
			}
			var got types.Tags
			if err := got.Scan(v); err != nil {
				t.Fatalf("Scan() error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("round-trip = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestTags_Scan_Sources verifies Scan accepts NULL, []byte, and string inputs.
func TestTags_Scan_Sources(t *testing.T) {
	want := types.Tags{"k": "v"}
	cases := map[string]any{
		"bytes":  []byte(`{"k":"v"}`),
		"string": `{"k":"v"}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			var got types.Tags
			if err := got.Scan(src); err != nil {
				t.Fatalf("Scan(%T) error: %v", src, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Scan(%T) = %#v, want %#v", src, got, want)
			}
		})
	}
}

// TestTags_Scan_NullAndEmpty verifies that NULL, empty payloads, and the JSON
// literal null all yield a nil Tags.
func TestTags_Scan_NullAndEmpty(t *testing.T) {
	cases := map[string]any{
		"nil":         nil,
		"empty bytes": []byte{},
		"json null":   []byte("null"),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			got := types.Tags{"stale": "value"} // ensure Scan resets prior state
			if err := got.Scan(src); err != nil {
				t.Fatalf("Scan() error: %v", err)
			}
			if got != nil {
				t.Errorf("Scan() = %#v, want nil", got)
			}
		})
	}
}

// TestTags_Scan_Errors verifies malformed JSON and unsupported source types are
// rejected.
func TestTags_Scan_Errors(t *testing.T) {
	cases := map[string]any{
		"malformed json":   []byte(`{"k":`),
		"non-object json":  []byte(`["a","b"]`),
		"unsupported type": 42,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			var got types.Tags
			if err := got.Scan(src); err == nil {
				t.Errorf("Scan(%v) = nil error, want error", src)
			}
		})
	}
}

// TestTags_Clone verifies Clone produces an independent copy and preserves nil.
func TestTags_Clone(t *testing.T) {
	if got := types.Tags(nil).Clone(); got != nil {
		t.Errorf("nil.Clone() = %#v, want nil", got)
	}

	orig := types.Tags{"a": "1"}
	clone := orig.Clone()
	clone["a"] = "changed"
	clone["b"] = "2"
	if orig["a"] != "1" {
		t.Errorf("Clone mutated original: orig[\"a\"] = %q, want %q", orig["a"], "1")
	}
	if _, ok := orig["b"]; ok {
		t.Error("Clone mutated original: unexpected key \"b\"")
	}
}

// TestTags_Merge verifies other wins on collision, inputs are not mutated, and
// an empty merge yields nil.
func TestTags_Merge(t *testing.T) {
	a := types.Tags{"x": "1", "keep": "a"}
	b := types.Tags{"x": "2", "y": "3"}

	got := a.Merge(b)
	want := types.Tags{"x": "2", "y": "3", "keep": "a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %#v, want %#v", got, want)
	}

	if a["x"] != "1" {
		t.Errorf("Merge mutated receiver: a[\"x\"] = %q, want %q", a["x"], "1")
	}
	if _, ok := b["keep"]; ok {
		t.Error("Merge mutated argument: unexpected key \"keep\" in b")
	}

	if got := types.Tags(nil).Merge(nil); got != nil {
		t.Errorf("nil.Merge(nil) = %#v, want nil", got)
	}
}

// TestTags_Filter verifies the predicate selects entries and that an empty
// result is nil.
func TestTags_Filter(t *testing.T) {
	tags := types.Tags{"env": "prod", "team": "platform", "tmp": "x"}

	got := tags.Filter(func(k, _ string) bool { return k != "tmp" })
	want := types.Tags{"env": "prod", "team": "platform"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Filter() = %#v, want %#v", got, want)
	}

	if got := tags.Filter(func(string, string) bool { return false }); got != nil {
		t.Errorf("Filter(none) = %#v, want nil", got)
	}
}

// TestTags_Keys verifies keys come back sorted.
func TestTags_Keys(t *testing.T) {
	tags := types.Tags{"c": "3", "a": "1", "b": "2"}
	got := tags.Keys()
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}

// TestTags_String verifies the deterministic sorted rendering, with empty -> "".
func TestTags_String(t *testing.T) {
	if got := types.Tags(nil).String(); got != "" {
		t.Errorf("nil.String() = %q, want empty", got)
	}
	tags := types.Tags{"team": "platform", "env": "prod"}
	if got, want := tags.String(), "env=prod,team=platform"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestTags_Validate_OK verifies well-formed tags pass.
func TestTags_Validate_OK(t *testing.T) {
	for name, tags := range map[string]types.Tags{
		"nil":     nil,
		"empty":   {},
		"typical": {"env": "prod", "team": "platform"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tags.Validate(); err != nil {
				t.Errorf("Validate() error = %v, want nil", err)
			}
		})
	}
}

// TestTags_Validate_Errors verifies structural violations are rejected.
func TestTags_Validate_Errors(t *testing.T) {
	tooMany := make(types.Tags, types.MaxTags+1)
	for i := 0; i <= types.MaxTags; i++ {
		tooMany["k"+strconv.Itoa(i)] = "v"
	}

	cases := map[string]types.Tags{
		"empty key":      {"": "v"},
		"oversize key":   {strings.Repeat("k", types.MaxKeyLen+1): "v"},
		"oversize value": {"k": strings.Repeat("v", types.MaxValueLen+1)},
		"invalid utf8":   {"k": string([]byte{0xff, 0xfe})},
		"too many":       tooMany,
	}
	for name, tags := range cases {
		t.Run(name, func(t *testing.T) {
			if err := tags.Validate(); err == nil {
				t.Errorf("Validate() = nil error, want error")
			}
		})
	}
}
