// Command schemagen prints the devedge-sdk framework-table DDL for a SQL dialect
// (default "postgres"), loading the canonical gormtx framework model set through
// ariga.io/atlas-provider-gorm. The atlas CLI runs it as an external_schema data
// source (see ../atlas.hcl) to (re)generate and drift-check the 0001_framework_init
// baseline. Build-time only — never imported by a service binary.
package main

import (
	"fmt"
	"os"

	"ariga.io/atlas-provider-gorm/gormschema"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

func main() {
	dialect := "postgres"
	if len(os.Args) > 1 {
		dialect = os.Args[1]
	}
	// The canonical framework model set is the SDK's single source of truth: the
	// outbox + idempotency tables plus the cell-based-development tables. Sourcing it
	// from gormtx's own MigrationModelsFor / CellMigrationModels keeps the baseline in
	// lockstep with the models the runtime stores read/write.
	models := append(
		gormtx.MigrationModelsFor(true /*outbox*/, true /*idempotency*/),
		gormtx.CellMigrationModels()...,
	)
	models = append(models, gormtx.RequestIdempotencyMigrationModels()...)
	stmts, err := gormschema.New(dialect).Load(models...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: load %s framework schema: %v\n", dialect, err)
		os.Exit(1)
	}
	fmt.Print(stmts)
}
