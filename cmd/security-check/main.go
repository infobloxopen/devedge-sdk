// Command security-check performs a static cross-reference between a compiled
// proto FileDescriptorSet and an authz rules JSON file. It reports every
// service method that lacks either an (infoblox.authz.v1.rule) annotation in
// the proto or a matching entry in the rules file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	authzv1 "github.com/infobloxopen/apis/proto/infoblox/authz/v1"
	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/seccheck"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the testable entry point. It parses args, runs the check, prints
// findings, and returns 1 if any Error-severity finding is produced.
func run(args []string) int {
	fs := flag.NewFlagSet("security-check", flag.ContinueOnError)
	descriptorPath := fs.String("descriptor", "", "path to binary proto FileDescriptorSet (required)")
	rulesPath := fs.String("rules", "", "path to JSON file containing []authz.MethodRule (optional)")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "security-check: %v\n", err)
		return 1
	}
	if *descriptorPath == "" {
		fmt.Fprintln(os.Stderr, "security-check: --descriptor is required")
		fs.Usage()
		return 1
	}

	raw, err := os.ReadFile(*descriptorPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "security-check: reading descriptor: %v\n", err)
		return 1
	}

	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(raw, fds); err != nil {
		fmt.Fprintf(os.Stderr, "security-check: unmarshalling descriptor: %v\n", err)
		return 1
	}

	var rules []authz.MethodRule
	if *rulesPath != "" {
		rb, err := os.ReadFile(*rulesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "security-check: reading rules: %v\n", err)
			return 1
		}
		if err := json.Unmarshal(rb, &rules); err != nil {
			fmt.Fprintf(os.Stderr, "security-check: parsing rules JSON: %v\n", err)
			return 1
		}
	}

	findings := checkDescriptor(fds, rules)
	hasError := false
	for _, f := range findings {
		fmt.Printf("[%s] %s: %s\n", f.Severity, f.Method, f.Message)
		if f.Severity == seccheck.Error {
			hasError = true
		}
	}
	if hasError {
		return 1
	}
	return 0
}

// checkDescriptor is the pure logic layer — it takes an already-parsed
// FileDescriptorSet and an optional rules slice, and returns all findings.
// Separating it from run() makes it straightforward to test without disk I/O.
func checkDescriptor(fds *descriptorpb.FileDescriptorSet, rules []authz.MethodRule) []seccheck.Finding {
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return []seccheck.Finding{{
			Method:   "(descriptor)",
			Severity: seccheck.Error,
			Message:  fmt.Sprintf("failed to build file registry: %v", err),
		}}
	}

	// Build a fast lookup set from the rules file (if provided).
	rulesProvided := len(rules) > 0
	rulesSet := make(map[string]bool, len(rules))
	for _, r := range rules {
		rulesSet[r.Method] = true
	}

	var findings []seccheck.Finding

	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		// Skip well-known google packages — they are not application services.
		if strings.HasPrefix(string(fd.Package()), "google.") {
			return true
		}

		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			methods := svc.Methods()
			for j := 0; j < methods.Len(); j++ {
				md := methods.Get(j)
				fullMethod := fmt.Sprintf("/%s/%s", svc.FullName(), md.Name())

				hasAnnotation := proto.HasExtension(md.Options(), authzv1.E_Rule)
				inRulesFile := rulesSet[fullMethod]

				switch {
				case hasAnnotation && rulesProvided && !inRulesFile:
					// Annotated in proto but missing from the rules file.
					findings = append(findings, seccheck.Finding{
						Method:   fullMethod,
						Severity: seccheck.Notice,
						Message:  "method has proto annotation but is absent from the rules file",
					})
				case !hasAnnotation && rulesProvided && inRulesFile:
					// In the rules file but not annotated in the proto.
					findings = append(findings, seccheck.Finding{
						Method:   fullMethod,
						Severity: seccheck.Notice,
						Message:  "method is in the rules file but lacks a proto annotation",
					})
				case !hasAnnotation && !inRulesFile:
					// Missing from both sources — hard error.
					msg := "method has no authz annotation in proto"
					if rulesProvided {
						msg = "method has no authz annotation in proto and is absent from the rules file"
					}
					findings = append(findings, seccheck.Finding{
						Method:   fullMethod,
						Severity: seccheck.Error,
						Message:  msg,
					})
				// hasAnnotation && (!rulesProvided || inRulesFile) → all good, no finding.
				}
			}
		}
		return true
	})

	return findings
}
