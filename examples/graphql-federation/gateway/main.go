// Command gateway runs the F042 cross-service GraphQL federation gateway over
// the sample region + asset services. In the default all-in-one mode it starts
// both services in-process on ephemeral ports, seeds the demo dataset, builds
// the GraphQL schema (Asset with a region edge -> Region), and serves the
// GraphQL HTTP endpoint — one command, then curl a federated query.
//
//	go run ./gateway                       # all-in-one on :8080
//	go run ./gateway -http :9000           # custom port
//	go run ./gateway -region :9101 -asset :9102 -all-in-one=false  # dial running services
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/infobloxopen/devedge-sdk/examples/graphql-federation/internal/svc"
)

func main() {
	httpAddr := flag.String("http", ":8080", "GraphQL HTTP listen address")
	allInOne := flag.Bool("all-in-one", true, "start the region + asset services in-process and seed the demo data")
	regionAddr := flag.String("region", "", "region service gRPC address (used when -all-in-one=false)")
	assetAddr := flag.String("asset", "", "asset service gRPC address (used when -all-in-one=false)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	rAddr, aAddr := *regionAddr, *assetAddr

	if *allInOne {
		region, err := svc.StartRegion(ctx, ":0", "gw_region")
		if err != nil {
			log.Fatalf("start region: %v", err)
		}
		defer region.Stop()
		asset, err := svc.StartAsset(ctx, ":0", "gw_asset")
		if err != nil {
			log.Fatalf("start asset: %v", err)
		}
		defer asset.Stop()
		rAddr, aAddr = region.Addr, asset.Addr
		log.Printf("region service on %s, asset service on %s (in-process)", rAddr, aAddr)

		if err := svc.Seed(ctx, rAddr, aAddr); err != nil {
			log.Fatalf("seed demo data: %v", err)
		}
		log.Printf("seeded %d regions + %d assets", len(svc.DemoRegions), len(svc.DemoAssets))
	}

	if rAddr == "" || aAddr == "" {
		log.Fatal("gateway needs region + asset addresses: use all-in-one (default) or pass -region and -asset")
	}

	gw, err := svc.NewGateway(rAddr, aAddr)
	if err != nil {
		log.Fatalf("build gateway: %v", err)
	}
	defer gw.Close()

	mux := http.NewServeMux()
	mux.Handle("/graphql", gw.Handler())

	srv := &http.Server{Addr: *httpAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	log.Printf("GraphQL gateway listening on %s/graphql", *httpAddr)
	log.Printf(`try: curl -s %s/graphql -H 'X-Account-Id: acme' -H 'Content-Type: application/json' -d '{"query":"{ assets { id name region { id name } } }"}'`, *httpAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http serve: %v", err)
	}
}
