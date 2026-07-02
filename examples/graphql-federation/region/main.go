// Command region runs the sample RegionService (the reference TARGET, serving
// the guaranteed AIP-137 BatchGetRegions) over ent + in-memory sqlite on a real
// listener. Run it, note the printed gRPC address, and pass it to the gateway's
// -region flag.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/infobloxopen/devedge-sdk/examples/graphql-federation/internal/svc"
)

func main() {
	addr := flag.String("addr", ":9101", "gRPC listen address (use :0 for an ephemeral port)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	service, err := svc.StartRegion(ctx, *addr, "region_demo")
	if err != nil {
		log.Fatalf("start region service: %v", err)
	}
	defer service.Stop()

	log.Printf("region service listening (gRPC) on %s", service.Addr)
	log.Printf("export REGION_ADDR=%s", service.Addr)
	<-ctx.Done()
	log.Printf("region service shutting down")
}
