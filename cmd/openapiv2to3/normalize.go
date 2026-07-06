package main

// gateway-v1 compat NORMALIZATIONS (WS-035): two source-swagger heritage
// defects that the old protoc-gen-swagger / atlas toolchains bake into their
// output and that hard-fail strict downstream client generators (oapi-codegen,
// tsc-on-ng-openapi-gen). Both run only under -compat=gateway-v1; the default
// (gateway-v2) path is untouched and stays byte-identical.
//
//  (a) format sanitization — drop `format` values that are invalid per the
//      OpenAPI Specification for the schema's `type` (at minimum
//      `format: boolean` on a boolean, which OpenAPI does not define and which
//      oapi-codegen rejects). Applied to schema properties AND parameters.
//  (b) path-param name restoration — swagger path flattening can collapse two
//      distinct template variables to the same name (e.g.
//      /groups/{group_id}/users/{id} → /groups/{id}/users/{id}), which is both
//      illegal OpenAPI (duplicate parameter) and un-generatable. For a matched
//      operation the true, unique names are recovered from the google.api.http
//      rule; otherwise the collision is broken mechanically ({id},{id2}).

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// --- (a) format sanitization -------------------------------------------------

// badFormatsByType lists `format` values that are invalid per the OpenAPI
// Specification for a given `type`. Legacy protoc-gen-swagger emits
// `format: boolean` on boolean schemas — OpenAPI defines no format for boolean,
// and strict generators (oapi-codegen) abort on it. The table is intentionally
// small and additive: a bad pair drops only the `format`; `type` and every
// other keyword are preserved, so the API contract is unchanged.
var badFormatsByType = map[string]map[string]bool{
	"boolean": {"boolean": true},
}

// sanitizeFormats drops invalid type/format pairs from every schema reachable
// in the document (component schemas + parameter/body/response schemas),
// counting each drop in the coverage report. Compat-mode only.
func sanitizeFormats(doc *openapi3.T, rep *coverageReport) {
	visited := map[*openapi3.Schema]bool{}
	var walk func(ref *openapi3.SchemaRef)
	walk = func(ref *openapi3.SchemaRef) {
		if ref == nil || ref.Value == nil || visited[ref.Value] {
			return
		}
		visited[ref.Value] = true
		s := ref.Value
		if s.Format != "" && s.Type != nil {
			for _, t := range s.Type.Slice() {
				if bad, ok := badFormatsByType[t]; ok && bad[s.Format] {
					rep.FormatsSanitized++
					s.Format = ""
					break
				}
			}
		}
		for _, p := range s.Properties {
			walk(p)
		}
		walk(s.Items)
		if s.AdditionalProperties.Schema != nil {
			walk(s.AdditionalProperties.Schema)
		}
		for _, sub := range s.AllOf {
			walk(sub)
		}
		for _, sub := range s.AnyOf {
			walk(sub)
		}
		for _, sub := range s.OneOf {
			walk(sub)
		}
		walk(s.Not)
	}

	walkParams := func(params openapi3.Parameters) {
		for _, pr := range params {
			if pr.Value == nil {
				continue
			}
			walk(pr.Value.Schema)
			walkContent(pr.Value.Content, walk)
		}
	}

	if doc.Components != nil {
		for _, ref := range doc.Components.Schemas {
			walk(ref)
		}
		for _, pr := range doc.Components.Parameters {
			if pr.Value != nil {
				walk(pr.Value.Schema)
				walkContent(pr.Value.Content, walk)
			}
		}
	}
	if doc.Paths == nil {
		return
	}
	for _, item := range doc.Paths.Map() {
		walkParams(item.Parameters)
		for _, op := range item.Operations() {
			walkParams(op.Parameters)
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				walkContent(op.RequestBody.Value.Content, walk)
			}
			if op.Responses == nil {
				continue
			}
			for _, rr := range op.Responses.Map() {
				if rr.Value == nil {
					continue
				}
				walkContent(rr.Value.Content, walk)
				for _, hr := range rr.Value.Headers {
					if hr.Value != nil {
						walk(hr.Value.Schema)
					}
				}
			}
		}
	}
}

func walkContent(c openapi3.Content, walk func(*openapi3.SchemaRef)) {
	for _, mt := range c {
		if mt != nil {
			walk(mt.Schema)
		}
	}
}

// --- (b) path-param name restoration -----------------------------------------

// pathParamCoverage counts the path-template repairs in the coverage report.
type pathParamCoverage struct {
	Restored int            `json:"restored"` // templates repaired from the proto rule's names
	Deduped  int            `json:"deduped"`  // templates de-duplicated mechanically ({id},{id2})
	Details  []pathParamFix `json:"details,omitempty"`
}

type pathParamFix struct {
	From   string `json:"from"`
	To     string `json:"to,omitempty"`
	Kind   string `json:"kind"`             // "restored" | "deduped" | "mismatch"
	Reason string `json:"reason,omitempty"` // set for "mismatch" (rewrite skipped)
}

// templateVarNames returns the variable names of a path template in order. For a
// deep/multi-segment variable ({name=a/**}) it keeps the leaf name — the part
// before '=' — matching how the swagger flattener labels such a segment.
func templateVarNames(raw string) []string {
	var names []string
	for i := 0; i < len(raw); {
		if raw[i] != '{' {
			i++
			continue
		}
		j := strings.IndexByte(raw[i:], '}')
		if j < 0 {
			break
		}
		inner := raw[i+1 : i+j]
		if eq := strings.IndexByte(inner, '='); eq >= 0 {
			inner = inner[:eq]
		}
		names = append(names, inner)
		i += j + 1
	}
	return names
}

// rewriteTemplate replaces the i-th {var} of a path template with
// {newNames[i]}, leaving every literal segment and any trailing custom-verb
// suffix intact.
func rewriteTemplate(raw string, newNames []string) string {
	var b strings.Builder
	k := 0
	for i := 0; i < len(raw); {
		if raw[i] != '{' {
			b.WriteByte(raw[i])
			i++
			continue
		}
		j := strings.IndexByte(raw[i:], '}')
		if j < 0 {
			b.WriteString(raw[i:])
			break
		}
		if k < len(newNames) {
			b.WriteString("{")
			b.WriteString(newNames[k])
			b.WriteString("}")
		} else {
			b.WriteString(raw[i : i+j+1])
		}
		k++
		i += j + 1
	}
	return b.String()
}

func hasDupVars(vars []string) bool {
	seen := map[string]bool{}
	for _, v := range vars {
		if seen[v] {
			return true
		}
		seen[v] = true
	}
	return false
}

// mechanicalDedup suffixes repeated names with an incrementing index, keeping
// the first occurrence as-is: [id id] → [id id2], [id id id] → [id id2 id3].
func mechanicalDedup(vars []string) []string {
	seen := map[string]int{}
	out := make([]string, len(vars))
	for i, v := range vars {
		seen[v]++
		if seen[v] == 1 {
			out[i] = v
		} else {
			out[i] = fmt.Sprintf("%s%d", v, seen[v])
		}
	}
	return out
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// restorePathParams repairs swagger path templates whose variable names were
// flattened to duplicates (e.g. /groups/{id}/users/{id}) — a shape that hard-
// fails oapi-codegen ("duplicate local parameter") and compiles to duplicate
// identifiers on the TypeScript path. For a defective template whose operations
// matched a google.api.http rule it restores the rule's unique design-time names
// (the path key + every path parameter, positionally); with no matched rule (or
// a var-count mismatch) it de-duplicates the names mechanically so the emitted
// spec is at least buildable. Non-duplicated templates are left untouched — the
// repair fires only on the actual defect. Compat-mode only.
func restorePathParams(doc *openapi3.T, matches []opMatch, bindings []httpBinding, rep *coverageReport) {
	if doc.Paths == nil {
		return
	}
	// Proto var-name sequences claimed by surviving matched ops, per swagger path.
	protoByPath := map[string][][]string{}
	for _, m := range matches {
		if m.op == nil {
			continue
		}
		protoByPath[m.path] = append(protoByPath[m.path], templateVarNames(bindings[m.binding].raw))
	}

	keys := make([]string, 0, doc.Paths.Len())
	for k := range doc.Paths.Map() {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, pathKey := range keys {
		swaggerVars := templateVarNames(pathKey)
		if !hasDupVars(swaggerVars) {
			continue // only duplicated templates are defective
		}
		var target []string
		kind := ""
		if seqs := protoByPath[pathKey]; len(seqs) > 0 {
			ref := seqs[0]
			agree := true
			for _, s := range seqs[1:] {
				if !equalStrs(s, ref) {
					agree = false
					break
				}
			}
			switch {
			case !agree:
				rep.PathParams.Details = append(rep.PathParams.Details, pathParamFix{
					From: pathKey, Kind: "mismatch",
					Reason: "matched operations disagree on path variables",
				})
			case len(ref) != len(swaggerVars):
				rep.PathParams.Details = append(rep.PathParams.Details, pathParamFix{
					From: pathKey, Kind: "mismatch",
					Reason: fmt.Sprintf("proto rule has %d path vars, swagger template has %d", len(ref), len(swaggerVars)),
				})
			default:
				target, kind = ref, "restored"
			}
		}
		if kind == "" { // unmatched, or a mismatch we still make buildable
			target, kind = mechanicalDedup(swaggerVars), "deduped"
		}
		// A restore whose proto names are themselves not unique still needs a
		// mechanical pass so the emitted template is legal.
		if hasDupVars(target) {
			target, kind = mechanicalDedup(target), "deduped"
		}
		if equalStrs(target, swaggerVars) {
			continue
		}
		newKey := rewriteTemplate(pathKey, target)
		if newKey != pathKey && doc.Paths.Value(newKey) != nil {
			rep.PathParams.Details = append(rep.PathParams.Details, pathParamFix{
				From: pathKey, To: newKey, Kind: "mismatch",
				Reason: "restored path collides with an existing path; left unchanged",
			})
			continue
		}
		item := doc.Paths.Value(pathKey)
		renamePathParams(item.Parameters, target)
		for _, op := range item.Operations() {
			renamePathParams(op.Parameters, target)
		}
		doc.Paths.Delete(pathKey)
		doc.Paths.Set(newKey, item)
		rep.PathParams.Details = append(rep.PathParams.Details, pathParamFix{From: pathKey, To: newKey, Kind: kind})
		if kind == "restored" {
			rep.PathParams.Restored++
		} else {
			rep.PathParams.Deduped++
		}
	}
	sort.Slice(rep.PathParams.Details, func(i, j int) bool {
		if rep.PathParams.Details[i].From != rep.PathParams.Details[j].From {
			return rep.PathParams.Details[i].From < rep.PathParams.Details[j].From
		}
		return rep.PathParams.Details[i].Kind < rep.PathParams.Details[j].Kind
	})
}

// renamePathParams renames the in:path parameters of one parameter list to the
// target variable names positionally (the k-th path parameter → target[k]).
func renamePathParams(params openapi3.Parameters, target []string) {
	k := 0
	for _, pr := range params {
		if pr.Value == nil || pr.Value.In != "path" {
			continue
		}
		if k < len(target) {
			pr.Value.Name = target[k]
		}
		k++
	}
}
