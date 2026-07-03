package slo

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScaffoldParity_DayOneEqualsGenerate proves the scaffold's day-one path
// (DefaultsForResource, given the freshly-scaffolded service's inputs) produces
// a byte-identical OpenSLO document to `de slo generate` (DefaultsFromOpenAPI on
// that service's enriched OpenAPI). This guards F070: the two derivation paths
// must agree on the slug, the method set, and every object name — otherwise
// running `make slo` with no proto change renames every SLI/SLO/AlertPolicy and
// orphans dashboards built on the day-one names.
//
// The fixture mirrors the scaffold proto template (Create/Get/List/Update/Delete
// for TicketService — no BatchGet, no Undelete). If proto.proto.tmpl gains an
// RPC without renderSLO's flags being updated to match, this test fails.
func TestScaffoldParity_DayOneEqualsGenerate(t *testing.T) {
	const (
		serviceShort = "TicketService"
		serviceFQN   = "ticketd.v1.TicketService"
	)

	dayOne, err := DefaultsForResource(ResourceDefaults{
		ServiceShort:    serviceShort,
		ServiceLabel:    serviceFQN,
		Resource:        "Ticket",
		ResourcePlural:  "Tickets",
		IncludeBatchGet: false,
		SoftDelete:      false,
	}, DefaultDeriveOptions())
	if err != nil {
		t.Fatalf("DefaultsForResource: %v", err)
	}

	data, err := os.ReadFile(filepath.Join("testdata", "ticketd.scaffold.openapi.yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	generated, err := DefaultsFromOpenAPI(data, serviceFQN, DefaultDeriveOptions())
	if err != nil {
		t.Fatalf("DefaultsFromOpenAPI: %v", err)
	}

	a, err := dayOne.Marshal()
	if err != nil {
		t.Fatalf("marshal day-one: %v", err)
	}
	b, err := generated.Marshal()
	if err != nil {
		t.Fatalf("marshal generated: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("day-one slo.yaml differs from `de slo generate` output.\n--- day-one ---\n%s\n--- generate ---\n%s", a, b)
	}

	// Spot-check the identity the finding called out: the slug is the gRPC short
	// name (ticket-service), not the binary name, and no phantom methods appear.
	if got := dayOne.SLOs[0].Spec.Service; got != "ticket-service" {
		t.Errorf("slug = %q, want ticket-service (the gRPC service short name)", got)
	}
	for _, sli := range dayOne.SLIs {
		if sli.Spec.RatioMetric == nil {
			continue
		}
		for _, m := range sli.Spec.RatioMetric.Good.Spec.Methods {
			if m == "BatchGetTickets" || m == "UndeleteTicket" {
				t.Errorf("phantom method %q in day-one SLI %q (scaffold proto has no such RPC)", m, sli.Metadata.Name)
			}
		}
	}
}
