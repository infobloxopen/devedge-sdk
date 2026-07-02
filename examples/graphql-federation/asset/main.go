// Command asset runs the sample AssetService (the reference SOURCE; each Asset's
// region_id names a Region served by the region service) over ent + in-memory
// sqlite on a real listener. Run it, note the printed gRPC address, and pass it
// to the gateway's -asset flag.
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
	addr := flag.String("addr", ":9102", "gRPC listen address (use :0 for an ephemeral port)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	service, err := svc.StartAsset(ctx, *addr, "asset_demo")
	if err != nil {
		log.Fatalf("start asset service: %v", err)
	}
	defer service.Stop()

	log.Printf("asset service listening (gRPC) on %s", service.Addr)
	log.Printf("export ASSET_ADDR=%s", service.Addr)
	<-ctx.Done()
	log.Printf("asset service shutting down")
}
