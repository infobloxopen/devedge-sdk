// Package resourcename provides AIP-122 resource name formatting and parsing.
// Patterns use {varname} placeholders, e.g. "widgets/{widget}" or
// "projects/{project}/widgets/{widget}".
// The package has no external dependencies.
package resourcename

import (
	"fmt"
	"strings"
)

// Parse extracts variable bindings from name given pattern.
//
//	Parse("widgets/{widget}", "widgets/abc123") → {"widget": "abc123"}, nil
//	Parse("projects/{project}/widgets/{widget}", "projects/p1/widgets/w2") → {"project":"p1","widget":"w2"}, nil
func Parse(pattern, name string) (map[string]string, error) {
	pSegs := strings.Split(pattern, "/")
	nSegs := strings.Split(name, "/")
	if len(pSegs) != len(nSegs) {
		return nil, fmt.Errorf("resourcename: %q does not match pattern %q: segment count %d != %d",
			name, pattern, len(nSegs), len(pSegs))
	}
	vars := make(map[string]string, len(pSegs)/2)
	for i, ps := range pSegs {
		if strings.HasPrefix(ps, "{") && strings.HasSuffix(ps, "}") {
			vars[ps[1:len(ps)-1]] = nSegs[i]
		} else if ps != nSegs[i] {
			return nil, fmt.Errorf("resourcename: segment %d: expected literal %q, got %q", i, ps, nSegs[i])
		}
	}
	return vars, nil
}

// Format constructs a resource name by substituting {var} placeholders in pattern.
//
//	Format("widgets/{widget}", {"widget": "abc123"}) → "widgets/abc123", nil
func Format(pattern string, vars map[string]string) (string, error) {
	segs := strings.Split(pattern, "/")
	out := make([]string, len(segs))
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			key := seg[1 : len(seg)-1]
			v, ok := vars[key]
			if !ok {
				return "", fmt.Errorf("resourcename: missing variable %q for pattern %q", key, pattern)
			}
			out[i] = v
		} else {
			out[i] = seg
		}
	}
	return strings.Join(out, "/"), nil
}

// IDFromName returns the value of the last variable in pattern extracted from name.
// It is a convenience wrapper for the common case where the resource ID is the
// final path segment.
//
//	IDFromName("widgets/{widget}", "widgets/abc123") → "abc123", nil
//	IDFromName("projects/{project}/widgets/{widget}", "projects/p1/widgets/abc") → "abc", nil
func IDFromName(pattern, name string) (string, error) {
	vars, err := Parse(pattern, name)
	if err != nil {
		return "", err
	}
	lastVar := lastVarName(pattern)
	if lastVar == "" {
		return "", fmt.Errorf("resourcename: no variable found in pattern %q", pattern)
	}
	return vars[lastVar], nil
}

// lastVarName returns the name of the last {var} placeholder in pattern.
func lastVarName(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		s := segs[i]
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			return s[1 : len(s)-1]
		}
	}
	return ""
}

// IDVarName returns the name of the last variable in pattern.
// Useful for code generators that need to know the ID variable name.
func IDVarName(pattern string) string {
	return lastVarName(pattern)
}
