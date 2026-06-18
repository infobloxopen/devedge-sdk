// Package types provides reusable, ORM-agnostic field types for Infoblox
// resources. The types depend only on the standard library so they can be
// embedded in core models without pulling in a database driver or an ORM.
//
// The Tags type mirrors the custom-field-type convention used across Infoblox
// (e.g. the Jsonb/Inet types in infobloxopen/protoc-gen-gorm): a small named
// type that implements database/sql/driver.Valuer and database/sql.Scanner so
// it persists transparently (as a JSON object, suitable for a JSON/JSONB
// column) and marshals naturally over the wire.
package types

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// Structural bounds enforced by Tags.Validate. These guard against malformed or
// abusive data regardless of policy; they are NOT semantic tag policy. Which
// keys and values are allowed — and which combinations are permitted — is owned
// by the external tag definition service (see TagValidator), not by this type.
const (
	// MaxKeyLen is the maximum length, in bytes, of a tag key.
	MaxKeyLen = 256
	// MaxValueLen is the maximum length, in bytes, of a tag value.
	MaxValueLen = 1024
	// MaxTags is the maximum number of entries a Tags map may hold.
	MaxTags = 256
)

// Tags is a set of key/value labels attached to a resource. It is plain data:
// the underlying map[string]string keeps SQL scanning/valuing, JSON marshaling,
// and the protobuf map<string, string> mapping all trivial. Tags carries no
// policy of its own — see TagValidator.
type Tags map[string]string

// compile-time assertion that Tags satisfies driver.Valuer. The pointer
// receiver Scan likewise satisfies database/sql.Scanner.
var _ driver.Valuer = Tags(nil)

// Value implements driver.Valuer. An empty or nil Tags persists as SQL NULL;
// otherwise the map is marshaled to a JSON object (with sorted keys), suitable
// for a json/jsonb column.
func (t Tags) Value() (driver.Value, error) {
	if len(t) == 0 {
		return nil, nil
	}
	return json.Marshal(t)
}

// Scan implements database/sql.Scanner. It accepts a SQL NULL (nil), a []byte,
// or a string holding a JSON object. A NULL or empty payload yields a nil Tags.
func (t *Tags) Scan(src any) error {
	if src == nil {
		*t = nil
		return nil
	}

	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("types: cannot scan %T into Tags", src)
	}

	if len(data) == 0 || string(data) == "null" {
		*t = nil
		return nil
	}

	m := make(map[string]string)
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("types: scanning Tags: %w", err)
	}
	*t = m
	return nil
}

// Clone returns an independent copy of t. Cloning nil returns nil.
func (t Tags) Clone() Tags {
	return maps.Clone(t)
}

// Merge returns a new Tags containing the entries of t overlaid with those of
// other; on a key collision, other wins. Neither receiver nor argument is
// mutated. The result is nil when the merge is empty.
func (t Tags) Merge(other Tags) Tags {
	if len(t) == 0 && len(other) == 0 {
		return nil
	}
	out := make(Tags, len(t)+len(other))
	maps.Copy(out, t)
	maps.Copy(out, other)
	return out
}

// Filter returns a new Tags holding only the entries for which keep reports
// true. The result is nil when nothing is kept.
func (t Tags) Filter(keep func(key, value string) bool) Tags {
	out := make(Tags)
	for k, v := range t {
		if keep(k, v) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Keys returns the tag keys in sorted order.
func (t Tags) Keys() []string {
	return slices.Sorted(maps.Keys(t))
}

// String returns a deterministic "key=value" rendering with keys sorted and
// pairs comma-separated, e.g. "env=prod,team=platform". Useful for logs.
func (t Tags) String() string {
	if len(t) == 0 {
		return ""
	}
	var b strings.Builder
	for i, k := range t.Keys() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(t[k])
	}
	return b.String()
}

// Validate checks structural well-formedness only: non-empty UTF-8 keys, valid
// UTF-8 values, per-key/value length limits, and a cap on the number of tags.
// It deliberately does NOT enforce semantic policy (allowed keys/values or
// permitted combinations) — that is the tag definition service's job, applied
// through TagValidator.
func (t Tags) Validate() error {
	if len(t) > MaxTags {
		return fmt.Errorf("types: too many tags: %d (max %d)", len(t), MaxTags)
	}
	for k, v := range t {
		switch {
		case k == "":
			return errors.New("types: tag key must not be empty")
		case !utf8.ValidString(k):
			return fmt.Errorf("types: tag key is not valid UTF-8: %q", k)
		case len(k) > MaxKeyLen:
			return fmt.Errorf("types: tag key %q exceeds max length %d", k, MaxKeyLen)
		case !utf8.ValidString(v):
			return fmt.Errorf("types: tag value for key %q is not valid UTF-8", k)
		case len(v) > MaxValueLen:
			return fmt.Errorf("types: tag value for key %q exceeds max length %d", k, MaxValueLen)
		}
	}
	return nil
}

// TagValidator is the seam to the external tag definition service, which owns
// semantic tag policy: which keys and values are allowed, and which
// combinations are permitted. It is intentionally not part of Tags — Tags is
// plain data, and policy lives in the service. The SDK ships this interface as
// the extension point; the service (or a client to it) implements it. Tags's
// own Validate covers structural well-formedness only.
type TagValidator interface {
	ValidateTags(ctx context.Context, t Tags) error
}
