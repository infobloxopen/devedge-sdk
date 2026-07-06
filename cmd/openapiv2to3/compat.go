package main

// gateway-v1 compatibility mode (WS-035): accept swagger 2.0 files emitted by
// the OLD grpc-gateway v1 / atlas toolchains (protoc-gen-swagger era) and still
// run the proto-authoritative enrichment pass. Those files differ from the
// gateway-v2 output the default path assumes in four ways, each handled here:
//
//  1. operationIds are NOT `Service_Method` — so operations are matched to
//     proto methods by (verb, path-template) from `google.api.http` rules,
//     prefix-tolerantly (swagger paths are often relative to a patched
//     basePath, e.g. proto `/host_app/v1/on_prem_hosts` vs swagger
//     basePath `/api/host_app/v1` + path `/on_prem_hosts`);
//  2. matched operations get a canonical proto-derived operationId
//     (`Service_Method`), the original preserved as x-legacy-operation-id;
//  3. schema properties may be snake_case (`json_names_for_fields=false`) —
//     auto-detected, overridable with -json-names;
//  4. definition names are the gw-v1/atlas style (bare or package-concat like
//     `identityUser`), resolved to proto messages through a tiered resolver.
//
// The default path's fail-loud losslessness gates degrade to a per-file
// COVERAGE REPORT (human-readable on stderr + <out>.coverage.json); -strict
// opts back into hard failure when anything is unmatched or ambiguous.

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	apiannotations "google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/infobloxopen/devedge-sdk/internal/aip"
)

// opMatch is one swagger operation matched to a proto http binding (index into
// the bindings slice). op is set to nil when a match is withdrawn as ambiguous.
type opMatch struct {
	op      *openapi3.Operation
	verb    string
	path    string
	binding int
}

// compatOptions carries the -compat sub-flags.
type compatOptions struct {
	// jsonNames is "auto", "snake", or "camel" (-json-names).
	jsonNames string
	// strict opts back into fail-loud: any unmatched/ambiguous item errors.
	strict bool
}

// --- coverage report ---------------------------------------------------------

type coverageReport struct {
	Input                 string         `json:"input,omitempty"`
	Mode                  string         `json:"mode"`
	JSONNames             string         `json:"jsonNames"`
	JSONNamesSource       string         `json:"jsonNamesSource"` // "flag" or "auto"
	Operations            opCoverage        `json:"operations"`
	Schemas               schemaCoverage    `json:"schemas"`
	Fields                fieldCoverage     `json:"fields"`
	FormatsSanitized      int               `json:"formatsSanitized"`
	PathParams            pathParamCoverage `json:"pathParams"`
	ProtoMethodsUnmatched []string          `json:"protoMethodsUnmatched,omitempty"`
}

type opCoverage struct {
	Total     int     `json:"total"`
	Matched   int     `json:"matched"`
	Unmatched []opGap `json:"unmatched,omitempty"`
	Ambiguous []opGap `json:"ambiguous,omitempty"`
}

type opGap struct {
	Verb        string   `json:"verb"`
	Path        string   `json:"path"`
	OperationID string   `json:"operationId,omitempty"`
	Reason      string   `json:"reason"`
	Candidates  []string `json:"candidates,omitempty"`
}

type schemaCoverage struct {
	Total     int           `json:"total"`
	Enriched  int           `json:"enriched"`
	WellKnown int           `json:"wellKnown"` // google.* messages (protobufAny, rpcStatus, …): recognized, deliberately not enriched
	Matched   []schemaMatch `json:"matched,omitempty"`
	Unmatched []schemaGap   `json:"unmatched,omitempty"`
	Ambiguous []schemaGap   `json:"ambiguous,omitempty"`
}

type schemaMatch struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Tier    string `json:"tier"`
}

type schemaGap struct {
	Name       string   `json:"name"`
	Reason     string   `json:"reason"`
	Candidates []string `json:"candidates,omitempty"`
}

type fieldCoverage struct {
	Enriched      int        `json:"enriched"`
	Skipped       int        `json:"skipped"`
	SkippedDetail []fieldGap `json:"skippedDetail,omitempty"`
}

type fieldGap struct {
	Schema   string `json:"schema"`
	Property string `json:"property"`
	Reason   string `json:"reason"`
}

func (r *coverageReport) fieldEnriched() { r.Fields.Enriched++ }

func (r *coverageReport) fieldSkipped(schema, property, reason string) {
	r.Fields.Skipped++
	r.Fields.SkippedDetail = append(r.Fields.SkippedDetail, fieldGap{Schema: schema, Property: property, Reason: reason})
}

// hasGaps reports whether anything in the file failed to match — the -strict
// failure condition (the report-mode analogue of the default path's gates).
func (r *coverageReport) hasGaps() bool {
	return len(r.Operations.Unmatched) > 0 || len(r.Operations.Ambiguous) > 0 ||
		len(r.Schemas.Unmatched) > 0 || len(r.Schemas.Ambiguous) > 0 ||
		r.Fields.Skipped > 0 || len(r.ProtoMethodsUnmatched) > 0
}

// print writes the human-readable coverage summary.
func (r *coverageReport) print(w io.Writer) {
	fmt.Fprintf(w, "openapiv2to3: gateway-v1 compat coverage for %s:\n", r.Input)
	fmt.Fprintf(w, "  json-names: %s (%s)\n", r.JSONNames, r.JSONNamesSource)
	fmt.Fprintf(w, "  operations: %d total, %d matched, %d unmatched, %d ambiguous\n",
		r.Operations.Total, r.Operations.Matched, len(r.Operations.Unmatched), len(r.Operations.Ambiguous))
	for _, g := range r.Operations.Unmatched {
		fmt.Fprintf(w, "    UNMATCHED %s %s (operationId %q): %s\n", g.Verb, g.Path, g.OperationID, g.Reason)
	}
	for _, g := range r.Operations.Ambiguous {
		fmt.Fprintf(w, "    AMBIGUOUS %s %s (operationId %q): %s\n      candidates: %s\n",
			g.Verb, g.Path, g.OperationID, g.Reason, strings.Join(g.Candidates, ", "))
	}
	fmt.Fprintf(w, "  schemas: %d total, %d enriched, %d well-known, %d unmatched, %d ambiguous\n",
		r.Schemas.Total, r.Schemas.Enriched, r.Schemas.WellKnown, len(r.Schemas.Unmatched), len(r.Schemas.Ambiguous))
	for _, g := range r.Schemas.Unmatched {
		fmt.Fprintf(w, "    UNMATCHED %q: %s\n", g.Name, g.Reason)
	}
	for _, g := range r.Schemas.Ambiguous {
		fmt.Fprintf(w, "    AMBIGUOUS %q: %s\n      candidates: %s\n", g.Name, g.Reason, strings.Join(g.Candidates, ", "))
	}
	fmt.Fprintf(w, "  fields: %d enriched, %d skipped\n", r.Fields.Enriched, r.Fields.Skipped)
	for _, g := range r.Fields.SkippedDetail {
		fmt.Fprintf(w, "    SKIPPED %s.%s: %s\n", g.Schema, g.Property, g.Reason)
	}
	fmt.Fprintf(w, "  formats sanitized: %d\n", r.FormatsSanitized)
	fmt.Fprintf(w, "  path params: %d restored, %d de-duplicated\n", r.PathParams.Restored, r.PathParams.Deduped)
	for _, f := range r.PathParams.Details {
		if f.Kind == "mismatch" {
			fmt.Fprintf(w, "    MISMATCH %s: %s\n", f.From, f.Reason)
			continue
		}
		fmt.Fprintf(w, "    %s %s -> %s\n", strings.ToUpper(f.Kind), f.From, f.To)
	}
	if n := len(r.ProtoMethodsUnmatched); n > 0 {
		fmt.Fprintf(w, "  proto methods with no swagger operation: %d\n", n)
		for _, m := range r.ProtoMethodsUnmatched {
			fmt.Fprintf(w, "    %s\n", m)
		}
	}
}

// --- (verb, path-template) matching ------------------------------------------

// pathSeg is one segment of a path template, normalized for shape matching:
// a variable segment ({id}, {widget.id}, {name=operations/*}) matches any other
// variable segment positionally; a literal must match exactly. lit carries the
// literal text — for a variable segment, only the trailing text after the
// closing brace (a custom-method suffix like ":archive"), so `{id}:archive`
// does not match `{id}:promote`.
type pathSeg struct {
	lit   string
	isVar bool
}

func splitTemplate(p string) []pathSeg {
	var out []pathSeg
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "{") {
			trail := ""
			if j := strings.LastIndexByte(s, '}'); j >= 0 && j+1 < len(s) {
				trail = s[j+1:]
			}
			out = append(out, pathSeg{isVar: true, lit: trail})
			continue
		}
		out = append(out, pathSeg{lit: s})
	}
	return out
}

func segsEqual(a, b []pathSeg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].isVar != b[i].isVar || a[i].lit != b[i].lit {
			return false
		}
	}
	return true
}

// Match ranks: prefer an exact template alignment over a suffix alignment, so
// a path that aligns exactly with one rule is not reported ambiguous merely
// because it is also a suffix of a longer rule elsewhere.
const (
	rankNone   = 0
	rankSuffix = 1
	rankExact  = 2
)

// matchRank compares a swagger path template against a proto rule template.
// Swagger paths are often RELATIVE to a patched basePath (e.g. proto
// `/host_app/v1/on_prem_hosts` vs basePath `/api/host_app/v1` + path
// `/on_prem_hosts`), so a suffix alignment in either direction counts.
func matchRank(swagger, rule []pathSeg) int {
	if len(swagger) == 0 || len(rule) == 0 {
		return rankNone
	}
	if segsEqual(swagger, rule) {
		return rankExact
	}
	if len(swagger) < len(rule) && segsEqual(swagger, rule[len(rule)-len(swagger):]) {
		return rankSuffix
	}
	if len(rule) < len(swagger) && segsEqual(swagger[len(swagger)-len(rule):], rule) {
		return rankSuffix
	}
	return rankNone
}

// --- proto index ---------------------------------------------------------------

// httpBinding is one (verb, path template) a proto method is REST-exposed at.
// A method's top-level google.api.http rule is idx 0; each additional_binding
// counts as its own binding (idx 1..).
type httpBinding struct {
	svc    protoreflect.ServiceDescriptor
	method protoreflect.MethodDescriptor
	idx    int
	verb   string // lower-case
	raw    string
	segs   []pathSeg
}

// String identifies the binding in reports and ambiguity errors.
func (b httpBinding) String() string {
	s := fmt.Sprintf("%s.%s (%s %s", b.svc.FullName(), b.method.Name(), b.verb, b.raw)
	if b.idx > 0 {
		s += fmt.Sprintf(", additional_binding[%d]", b.idx)
	}
	return s + ")"
}

// canonicalOperationID synthesizes the gateway-v2-style operationId for the
// binding: `Service_Method`, with the gateway's `2`, `3`, … suffix for
// additional bindings (both gateway generations number extra bindings so the
// ids stay unique within the document).
func (b httpBinding) canonicalOperationID() string {
	id := fmt.Sprintf("%s_%s", b.svc.Name(), b.method.Name())
	if b.idx > 0 {
		id = fmt.Sprintf("%s%d", id, b.idx+1)
	}
	return id
}

type svcFacts struct {
	res        protoreflect.MessageDescriptor
	softDelete bool
}

// collectBindings walks every REST-exposed method in the FDS and returns its
// http bindings plus per-service resource facts for AIP classification.
func collectBindings(files *protoregistry.Files) ([]httpBinding, map[protoreflect.FullName]svcFacts) {
	var bindings []httpBinding
	facts := map[protoreflect.FullName]svcFacts{}
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(fd.Package()), "google.") {
			return true
		}
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			sd := svcs.Get(i)
			res := aip.DetectServiceResource(sd)
			facts[sd.FullName()] = svcFacts{
				res:        res,
				softDelete: res != nil && aip.MessageFacts(res).SoftDelete,
			}
			methods := sd.Methods()
			for j := 0; j < methods.Len(); j++ {
				md := methods.Get(j)
				if !proto.HasExtension(md.Options(), apiannotations.E_Http) {
					continue
				}
				rule, _ := proto.GetExtension(md.Options(), apiannotations.E_Http).(*apiannotations.HttpRule)
				if rule == nil {
					continue
				}
				rules := append([]*apiannotations.HttpRule{rule}, rule.GetAdditionalBindings()...)
				for idx, r := range rules {
					verb, path := ruleVerbPath(r)
					if verb == "" || path == "" {
						continue
					}
					bindings = append(bindings, httpBinding{
						svc: sd, method: md, idx: idx,
						verb: verb, raw: path, segs: splitTemplate(path),
					})
				}
			}
		}
		return true
	})
	sort.Slice(bindings, func(i, j int) bool {
		if a, b := bindings[i].method.FullName(), bindings[j].method.FullName(); a != b {
			return a < b
		}
		return bindings[i].idx < bindings[j].idx
	})
	return bindings, facts
}

// ruleVerbPath extracts the (lower-case verb, path template) of one HttpRule.
func ruleVerbPath(r *apiannotations.HttpRule) (string, string) {
	switch p := r.GetPattern().(type) {
	case *apiannotations.HttpRule_Get:
		return "get", p.Get
	case *apiannotations.HttpRule_Put:
		return "put", p.Put
	case *apiannotations.HttpRule_Post:
		return "post", p.Post
	case *apiannotations.HttpRule_Delete:
		return "delete", p.Delete
	case *apiannotations.HttpRule_Patch:
		return "patch", p.Patch
	case *apiannotations.HttpRule_Custom:
		if c := p.Custom; c != nil {
			return strings.ToLower(c.GetKind()), c.GetPath()
		}
	}
	return "", ""
}

// --- schema-name resolution ------------------------------------------------------

// schemaResolver maps gw-v1/atlas definition names back to proto messages.
// protoc-gen-swagger (v1) definition names are package-derived or bare message
// names — often `pkgMessage` concatenations like `identityUser` or
// `apiPageInfo` — vs gateway-v2's disambiguated style. Resolution tiers, in
// order: exact fully-qualified name, package-prefix concat (last package
// segment + flattened message name), bare (flattened) message name if unique
// across the FDS, then case-insensitive variants of the three. Ambiguity at a
// tier stops resolution (skip + report); it never falls through to a weaker
// tier, which could silently pick the wrong message.
type schemaResolver struct {
	byFQN                                  map[string]protoreflect.MessageDescriptor
	byConcat, byBare                       map[string][]protoreflect.MessageDescriptor
	byFQNLower, byConcatLower, byBareLower map[string][]protoreflect.MessageDescriptor
}

func buildSchemaResolver(files *protoregistry.Files) *schemaResolver {
	r := &schemaResolver{
		byFQN:         map[string]protoreflect.MessageDescriptor{},
		byConcat:      map[string][]protoreflect.MessageDescriptor{},
		byBare:        map[string][]protoreflect.MessageDescriptor{},
		byFQNLower:    map[string][]protoreflect.MessageDescriptor{},
		byConcatLower: map[string][]protoreflect.MessageDescriptor{},
		byBareLower:   map[string][]protoreflect.MessageDescriptor{},
	}
	add := func(m map[string][]protoreflect.MessageDescriptor, k string, md protoreflect.MessageDescriptor) {
		m[k] = append(m[k], md)
	}
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		pkg := string(fd.Package())
		last := pkg
		if i := strings.LastIndex(pkg, "."); i >= 0 {
			last = pkg[i+1:]
		}
		var walk func(prefix string, md protoreflect.MessageDescriptor)
		walk = func(prefix string, md protoreflect.MessageDescriptor) {
			if md.IsMapEntry() {
				return
			}
			// Nested messages flatten (Parent + Nested), mirroring how the
			// gateway generators name nested definitions.
			flat := prefix + string(md.Name())
			fqn := string(md.FullName())
			r.byFQN[fqn] = md
			add(r.byFQNLower, strings.ToLower(fqn), md)
			add(r.byConcat, last+flat, md)
			add(r.byConcatLower, strings.ToLower(last+flat), md)
			add(r.byBare, flat, md)
			add(r.byBareLower, strings.ToLower(flat), md)
			nested := md.Messages()
			for i := 0; i < nested.Len(); i++ {
				walk(flat, nested.Get(i))
			}
		}
		msgs := fd.Messages()
		for i := 0; i < msgs.Len(); i++ {
			walk("", msgs.Get(i))
		}
		return true
	})
	return r
}

// resolve maps one definition name to a proto message. It returns the match and
// its tier, or a schemaGap describing why the name did not resolve.
func (r *schemaResolver) resolve(name string) (protoreflect.MessageDescriptor, string, *schemaGap) {
	if md, ok := r.byFQN[name]; ok {
		return md, "fqn", nil
	}
	tiers := []struct {
		tier string
		hits []protoreflect.MessageDescriptor
	}{
		{"package-concat", r.byConcat[name]},
		{"bare", r.byBare[name]},
		{"fqn (case-insensitive)", r.byFQNLower[strings.ToLower(name)]},
		{"package-concat (case-insensitive)", r.byConcatLower[strings.ToLower(name)]},
		{"bare (case-insensitive)", r.byBareLower[strings.ToLower(name)]},
	}
	for _, t := range tiers {
		switch len(t.hits) {
		case 0:
			continue
		case 1:
			return t.hits[0], t.tier, nil
		default:
			names := make([]string, len(t.hits))
			for i, md := range t.hits {
				names[i] = string(md.FullName())
			}
			sort.Strings(names)
			return nil, "", &schemaGap{
				Name:       name,
				Reason:     fmt.Sprintf("%s name matches multiple messages", t.tier),
				Candidates: names,
			}
		}
	}
	return nil, "", &schemaGap{Name: name, Reason: "no proto message found"}
}

// --- json-names auto-detection ------------------------------------------------

// detectJSONNames probes the resolved (schema, message) pairs: for every
// property whose snake and camel spellings differ, it votes for the spelling
// the document actually uses. A majority of snake votes selects snake mode
// (json_names_for_fields=false emitters); otherwise camel (the proto JSON and
// gateway-v2 default).
func detectJSONNames(doc *openapi3.T, resolved map[string]protoreflect.MessageDescriptor) propKeyMode {
	snakeVotes, camelVotes := 0, 0
	for name, md := range resolved {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			continue
		}
		snakeSet := map[string]bool{}
		camelSet := map[string]bool{}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			snakeSet[string(fd.Name())] = true
			camelSet[string(fd.JSONName())] = true
		}
		for prop := range ref.Value.Properties {
			inSnake, inCamel := snakeSet[prop], camelSet[prop]
			switch {
			case inSnake && !inCamel:
				snakeVotes++
			case inCamel && !inSnake:
				camelVotes++
			}
		}
	}
	if snakeVotes > camelVotes {
		return keySnake
	}
	return keyCamel
}

// --- the compat enrichment pass -------------------------------------------------

// enrichCompat is the gateway-v1 analogue of enrich: same proto-authoritative
// enrichment, but operations are matched by (verb, path-template), operationIds
// are canonicalized, properties may be keyed snake_case, and definition names go
// through the tiered resolver. It ALWAYS returns a coverage report (for stderr +
// <out>.coverage.json); the error is non-nil only under opts.strict when the
// report has gaps.
func enrichCompat(doc *openapi3.T, files *protoregistry.Files, opts compatOptions) (*coverageReport, error) {
	rep := &coverageReport{Mode: "gateway-v1"}

	// (a) Drop invalid type/format pairs (e.g. legacy `format: boolean`) before
	// enrichment so the emitted spec is generatable by strict clients.
	sanitizeFormats(doc, rep)

	bindings, facts := collectBindings(files)
	resolver := buildSchemaResolver(files)

	// Resolve every definition name to a proto message (or report why not).
	resolved := map[string]protoreflect.MessageDescriptor{} // enrichable (non-google) matches
	var schemaNames []string
	if doc.Components != nil {
		for name := range doc.Components.Schemas {
			schemaNames = append(schemaNames, name)
		}
	}
	sort.Strings(schemaNames)
	rep.Schemas.Total = len(schemaNames)
	for _, name := range schemaNames {
		md, tier, gap := resolver.resolve(name)
		if gap != nil {
			if gap.Candidates != nil {
				rep.Schemas.Ambiguous = append(rep.Schemas.Ambiguous, *gap)
			} else {
				rep.Schemas.Unmatched = append(rep.Schemas.Unmatched, *gap)
			}
			continue
		}
		if strings.HasPrefix(string(md.ParentFile().Package()), "google.") {
			// Gateway-injected well-known types (protobufAny, rpcStatus, …) are
			// not app contract: recognized so they don't count as gaps, and
			// deliberately left un-enriched (mirrors the default path).
			rep.Schemas.WellKnown++
			continue
		}
		rep.Schemas.Matched = append(rep.Schemas.Matched, schemaMatch{
			Name: name, Message: string(md.FullName()), Tier: tier,
		})
		resolved[name] = md
	}

	// Pick the property key mode: flag override, or probe the document.
	mode := keyCamel
	switch opts.jsonNames {
	case "snake":
		mode, rep.JSONNames, rep.JSONNamesSource = keySnake, "snake", "flag"
	case "camel":
		mode, rep.JSONNames, rep.JSONNamesSource = keyCamel, "camel", "flag"
	default: // auto
		mode = detectJSONNames(doc, resolved)
		rep.JSONNames, rep.JSONNamesSource = "camel", "auto"
		if mode == keySnake {
			rep.JSONNames = "snake"
		}
	}

	// Enrich resolved schemas; gate failures become report entries.
	for _, m := range rep.Schemas.Matched {
		ref := doc.Components.Schemas[m.Name]
		if ref == nil || ref.Value == nil {
			continue
		}
		if err := enrichSchemaCore(m.Name, ref.Value, resolved[m.Name], mode, rep); err != nil {
			return rep, err // unreachable with rep != nil; defensive
		}
		rep.Schemas.Enriched++
	}

	// Match every swagger (verb, path) against the proto http bindings.
	var matches []opMatch
	var pathKeys []string
	pathsMap := map[string]*openapi3.PathItem{}
	if doc.Paths != nil {
		pathsMap = doc.Paths.Map()
	}
	for p := range pathsMap {
		pathKeys = append(pathKeys, p)
	}
	sort.Strings(pathKeys)
	for _, path := range pathKeys {
		swSegs := splitTemplate(path)
		ops := pathsMap[path].Operations()
		verbs := make([]string, 0, len(ops))
		for v := range ops {
			verbs = append(verbs, v)
		}
		sort.Strings(verbs)
		for _, httpVerb := range verbs {
			op := ops[httpVerb]
			verb := strings.ToLower(httpVerb)
			rep.Operations.Total++

			best, bestRank := []int{}, rankNone
			for bi, b := range bindings {
				if b.verb != verb {
					continue
				}
				switch r := matchRank(swSegs, b.segs); {
				case r > bestRank:
					best, bestRank = []int{bi}, r
				case r == bestRank && r > rankNone:
					best = append(best, bi)
				}
			}
			switch len(best) {
			case 0:
				rep.Operations.Unmatched = append(rep.Operations.Unmatched, opGap{
					Verb: verb, Path: path, OperationID: op.OperationID,
					Reason: "no google.api.http rule matches this (verb, path)",
				})
			case 1:
				matches = append(matches, opMatch{op: op, verb: verb, path: path, binding: best[0]})
			default:
				candidates := make([]string, len(best))
				for i, bi := range best {
					candidates[i] = bindings[bi].String()
				}
				rep.Operations.Ambiguous = append(rep.Operations.Ambiguous, opGap{
					Verb: verb, Path: path, OperationID: op.OperationID,
					Reason:     "matches multiple proto methods",
					Candidates: candidates,
				})
			}
		}
	}

	// Reverse ambiguity: one proto binding claimed by several swagger paths.
	byBinding := map[int][]int{} // binding index → indexes into matches
	for mi, m := range matches {
		byBinding[m.binding] = append(byBinding[m.binding], mi)
	}
	matchedBindings := map[int]bool{}
	for bi, ms := range byBinding {
		if len(ms) == 1 {
			matchedBindings[bi] = true
			continue
		}
		paths := make([]string, len(ms))
		for i, mi := range ms {
			paths[i] = matches[mi].verb + " " + matches[mi].path
		}
		sort.Strings(paths)
		for _, mi := range ms {
			m := matches[mi]
			rep.Operations.Ambiguous = append(rep.Operations.Ambiguous, opGap{
				Verb: m.verb, Path: m.path, OperationID: m.op.OperationID,
				Reason:     fmt.Sprintf("proto method %s matches multiple swagger operations", bindings[m.binding].String()),
				Candidates: paths,
			})
			matches[mi].op = nil // withdrawn
		}
	}
	sort.Slice(rep.Operations.Ambiguous, func(i, j int) bool {
		a, b := rep.Operations.Ambiguous[i], rep.Operations.Ambiguous[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Verb < b.Verb
	})

	// Canonicalize + enrich the surviving matches.
	for _, m := range matches {
		if m.op == nil {
			continue
		}
		b := bindings[m.binding]
		rep.Operations.Matched++
		if legacy := m.op.OperationID; legacy != "" {
			setExt(&m.op.Extensions, "x-legacy-operation-id", legacy)
		}
		m.op.OperationID = b.canonicalOperationID()
		f := facts[b.svc.FullName()]
		std := aip.ClassifyMethod(b.method, f.res, f.softDelete)
		setExt(&m.op.Extensions, "x-aip-method", std.String())
		if std == aip.MethodList {
			if pg := paginationExt(b.method); pg != nil {
				setExt(&m.op.Extensions, "x-aip-pagination", pg)
			}
		}
	}

	// (b) Restore duplicate-collapsed path-template variable names from the
	// matched proto rules (mechanical de-dup where no rule matched), so path
	// templates carry unique parameter names.
	restorePathParams(doc, matches, bindings, rep)

	// The default path's "every REST-exposed method has an operation" gate,
	// degraded to a report entry.
	for bi, b := range bindings {
		if !matchedBindings[bi] {
			rep.ProtoMethodsUnmatched = append(rep.ProtoMethodsUnmatched, b.String())
		}
	}
	sort.Strings(rep.ProtoMethodsUnmatched)

	if opts.strict && rep.hasGaps() {
		return rep, fmt.Errorf("gateway-v1 compat under -strict: %d operations unmatched, %d ambiguous; %d schemas unmatched, %d ambiguous; %d fields skipped; %d proto methods without operations",
			len(rep.Operations.Unmatched), len(rep.Operations.Ambiguous),
			len(rep.Schemas.Unmatched), len(rep.Schemas.Ambiguous),
			rep.Fields.Skipped, len(rep.ProtoMethodsUnmatched))
	}
	return rep, nil
}
