// slogen turns a devedge service's API contract into reliability artifacts
// (WS-025): it derives GOOD default OpenSLO SLOs from an enriched OpenAPI doc,
// lints them through the fail-loud three-layer classifier, and renders them to
// Prometheus/Cortex rules, Grafana dashboards, or Loki LogQL rules.
//
// It is the standalone tool `de slo` orchestrates via a pinned `go run` (the
// WS-023 hermetic pattern). The CLI contract below is STABLE — `de` depends on
// it.
//
// Verbs:
//
//	slogen generate --openapi <path> [--service <name>] [--out slo.yaml]
//	slogen lint <file...> [--format json]
//	slogen render --target prometheus|grafana|loki --in <slo.yaml> --out <dir> [--preset-dir <dir>]
//	slogen kpis
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/infobloxopen/devedge-sdk/slo"
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "slogen",
		Short:         "Derive, lint, and project SLI/SLOs for a devedge service (WS-025).",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(generateCmd(), lintCmd(), renderCmd(), kpisCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "slogen:", err)
		os.Exit(1)
	}
}

func generateCmd() *cobra.Command {
	var openapiPath, service, out string
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Derive default OpenSLO SLOs from an enriched OpenAPI doc.",
		RunE: func(_ *cobra.Command, _ []string) error {
			if openapiPath == "" {
				return fmt.Errorf("--openapi is required")
			}
			data, err := os.ReadFile(openapiPath)
			if err != nil {
				return fmt.Errorf("read openapi: %w", err)
			}
			doc, err := slo.DefaultsFromOpenAPI(data, service, slo.DefaultDeriveOptions())
			if err != nil {
				return err
			}
			b, err := doc.Marshal()
			if err != nil {
				return err
			}
			if out == "" || out == "-" {
				_, err = os.Stdout.Write(b)
				return err
			}
			if err := os.WriteFile(out, b, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s (%d SLOs)\n", out, len(doc.SLOs))
			return nil
		},
	}
	cmd.Flags().StringVar(&openapiPath, "openapi", "", "path to the enriched OpenAPI YAML (WS-024)")
	cmd.Flags().StringVar(&service, "service", "", "rpc.service label value (proto FQN, e.g. orders.v1.OrderService)")
	cmd.Flags().StringVar(&out, "out", "slo.yaml", "output path (- for stdout)")
	return cmd
}

func lintCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "lint <file...>",
		Short: "Validate OpenSLO docs and run the fail-loud three-layer classifier.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var all slo.Findings
			for _, f := range args {
				data, err := os.ReadFile(f)
				if err != nil {
					return fmt.Errorf("read %s: %w", f, err)
				}
				doc, err := slo.Parse(data)
				if err != nil {
					return fmt.Errorf("%s: %w", f, err)
				}
				all = append(all, slo.Lint(doc)...)
			}
			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(all); err != nil {
					return err
				}
			} else {
				printFindings(all)
			}
			if all.HasError() {
				return fmt.Errorf("lint failed: %d error-severity finding(s)", countErrors(all))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	return cmd
}

func renderCmd() *cobra.Command {
	var target, in, out, presetDir string
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Project an OpenSLO doc to a backend (prometheus|grafana|loki).",
		RunE: func(_ *cobra.Command, _ []string) error {
			if target == "" {
				return fmt.Errorf("--target is required (prometheus|grafana|loki)")
			}
			if in == "" {
				return fmt.Errorf("--in is required")
			}
			data, err := os.ReadFile(in)
			if err != nil {
				return fmt.Errorf("read %s: %w", in, err)
			}
			doc, err := slo.Parse(data)
			if err != nil {
				return err
			}
			rendered, err := slo.Render(target, doc, slo.RenderOptions{PresetDir: presetDir})
			if err != nil {
				return err
			}
			if out == "" || out == "-" {
				for _, r := range rendered {
					os.Stdout.Write(r.Content)
				}
				return nil
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			for _, r := range rendered {
				p := filepath.Join(out, r.Filename)
				if err := os.WriteFile(p, r.Content, 0o644); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "wrote %s\n", p)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "prometheus|grafana|loki")
	cmd.Flags().StringVar(&in, "in", "", "input OpenSLO YAML")
	cmd.Flags().StringVar(&out, "out", "", "output directory (- for stdout)")
	cmd.Flags().StringVar(&presetDir, "preset-dir", "", "directory of <target>.tmpl emitter overrides (internal overlay seam)")
	return cmd
}

func kpisCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kpis",
		Short: "Print the Layer-0 API KPI reference (golden signals + RED + USE).",
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Print(slo.KPIReferenceText())
			return nil
		},
	}
}

func printFindings(fs slo.Findings) {
	if len(fs) == 0 {
		fmt.Println("OK: no findings.")
		return
	}
	for _, f := range fs {
		fmt.Printf("%-5s [%s] %s: %s\n", f.Severity, f.Kind, f.Object, f.Message)
	}
}

func countErrors(fs slo.Findings) int {
	n := 0
	for _, f := range fs {
		if f.Severity == slo.SeverityError {
			n++
		}
	}
	return n
}
