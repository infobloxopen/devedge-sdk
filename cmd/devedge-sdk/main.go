// Command devedge-sdk is the devedge-sdk developer CLI. Its first capability is
// scaffolding an apx-native service:
//
//	devedge-sdk new service <name> --resource <Resource> --backend gorm
//
// The noun-verb shape (`new service`) leaves room for `new resource` etc. later.
// See specs/028-apx-native-scaffold for the design (D-1).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/infobloxopen/devedge-sdk/cmd/devedge-sdk/internal/scaffold"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "devedge-sdk",
		Short:         "devedge-sdk developer CLI",
		Long:          "devedge-sdk is the developer CLI for the devedge SDK. Use `new service` to scaffold an apx-native, authz-gated, persisting service from one resource.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newNewCmd())
	return root
}

func newNewCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "new",
		Short: "Scaffold a new artifact (service, ...)",
	}
	c.AddCommand(newNewServiceCmd())
	return c
}

func newNewServiceCmd() *cobra.Command {
	var (
		resource   string
		backend    string
		module     string
		org        string
		dir        string
		noGenerate bool
		force      bool
		aggregate  bool
		deployTgts string
	)
	c := &cobra.Command{
		Use:   "service <name>",
		Short: "Scaffold an apx-native, authz-gated, persisting service",
		Long: `Scaffold a new service from a single resource.

The generated project:
  - declares its PUBLIC API as an apx app-role module (apx.yaml + apx-release.yml),
    with every RPC carrying an authz rule (fail-closed boot gate);
  - generates its PRIVATE implementation (models + repository + service scaffolding)
    with buf + the SDK plugins (git-ignored, engine deps in the consumer go.mod only);
  - builds, boots, persists, and passes its smoke test with zero hand-edits.

apx and buf must be on PATH.

The module path defaults to github.com/<org>/<name> (org defaults to infobloxopen).
If you are NOT publishing under github.com/infobloxopen, set --module to your own
path so the generated go.mod + imports are correct, e.g.
  --module github.com/you/orders`,
		Example: `  # Infoblox-internal (module defaults to github.com/infobloxopen/orders):
  devedge-sdk new service orders --resource Order --backend gorm

  # External users: set your own module path:
  devedge-sdk new service orders --resource Order --backend gorm --module github.com/you/orders`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			target := dir
			if target == "" {
				target = name
			}
			opts := scaffold.Options{
				Service:    name,
				Resource:   resource,
				Module:     module,
				Org:        org,
				Backend:    scaffold.Backend(backend),
				Dir:        target,
				NoGenerate: noGenerate,
				Force:      force,
				Aggregate:  aggregate,
				Deploy:     deployTgts,
			}
			m, err := scaffold.Generate(cmd.Context(), opts, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n✓ scaffolded %s service %q in %s\n", m.Backend, m.Service, target)

			// Note the codegen plugins the project depends on, so the user knows
			// what `make tools` installs and what `buf generate` runs. The SDK
			// plugins are pinned to %s; the public plugins come from their canonical
			// modules. (gorm uses protoc-gen-storage; ent uses protoc-gen-ent.)
			enginePlugin := "protoc-gen-storage"
			if m.Backend == scaffold.BackendEnt {
				enginePlugin = "protoc-gen-ent"
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"  codegen plugins (installed by `make tools`, run by `buf generate`):\n"+
					"    devedge-sdk @ %s: protoc-gen-devedge-authz, protoc-gen-svc, %s\n"+
					"    public:          protoc-gen-go, protoc-gen-go-grpc, protoc-gen-grpc-gateway\n",
				m.SDKVersion, enginePlugin)

			if noGenerate {
				fmt.Fprintf(cmd.OutOrStdout(), "  next: cd %s && make tools && make generate && make test\n", target)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  next: cd %s && make test   (or: make run)\n", target)
			}
			return nil
		},
	}
	c.Flags().StringVar(&resource, "resource", "", "singular resource type name (e.g. Order); defaults from the service name")
	c.Flags().StringVar(&backend, "backend", "gorm", "persistence backend: gorm|ent")
	c.Flags().StringVar(&module, "module", "", "Go module path; defaults to github.com/<org>/<name>. SET THIS for non-Infoblox modules (e.g. --module github.com/you/orders)")
	c.Flags().StringVar(&org, "org", "infobloxopen", "apx organization")
	c.Flags().StringVar(&dir, "dir", "", "target directory (defaults to the service name)")
	c.Flags().BoolVar(&noGenerate, "no-generate", false, "skip the first buf generate + go mod tidy")
	c.Flags().BoolVar(&force, "force", false, "scaffold into a non-empty directory")
	c.Flags().BoolVar(&aggregate, "aggregate", false, "scaffold the resource as a DDD aggregate root (owns a member resource) and wire the TxRunner + AggregateRepository + transactional-outbox Publisher/Dispatcher in main")
	c.Flags().StringVar(&deployTgts, "deploy", "", "deploy targets to render (comma-separated): k8s,compose (default both); \"none\" to skip. k8s emits a Flux HelmRelease+OCIRepository+values overlay (framework-owned Helm chart); compose emits docker-compose.yml")
	return c
}
