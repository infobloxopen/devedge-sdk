// Package svc bootstraps the two microservices of the F042 sample over ent +
// sqlite on real listeners. Each service runs a real server.Server with
// fail-closed authz (a DevAuthorizer + DevPrincipalFunc), so a request with no
// principal is denied per-service — the gateway never bypasses it (AC-5).
package svc

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"

	"github.com/infobloxopen/devedge-sdk/authz"
	"github.com/infobloxopen/devedge-sdk/authz/grpcauthz"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/server"
	"github.com/infobloxopen/devedge-sdk/testdata/federation/ent"
	_ "github.com/infobloxopen/devedge-sdk/testdata/federation/ent/runtime" // installs mixin validators + tenant interceptors
	"github.com/infobloxopen/devedge-sdk/testdata/federation/federationv1"
)

// modernc.org/sqlite registers under the driver name "sqlite"; entgo's ent.Open
// expects "sqlite3" (dialect.SQLite). Re-export the already-registered driver
// under the "sqlite3" alias so the sample can open ent over pure-Go sqlite with
// no cgo.
var _registerSQLite3Once sync.Once

func init() {
	_registerSQLite3Once.Do(func() {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			panic("graphql-federation sample: open sqlite driver: " + err.Error())
		}
		drv := db.Driver()
		_ = db.Close()
		for _, name := range sql.Drivers() {
			if name == "sqlite3" {
				return
			}
		}
		sql.Register("sqlite3", drv.(driver.Driver))
	})
}

// openEntClient opens an in-memory sqlite ent client and migrates the schema.
// Each service gets its OWN database (distinct cache name) so the two services
// are genuinely separate stores — the gateway composes across them.
func openEntClient(ctx context.Context, name string) (*ent.Client, error) {
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=foreign_keys(1)", name)
	client, err := ent.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open ent (%s): %w", name, err)
	}
	if err := client.Schema.Create(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("migrate (%s): %w", name, err)
	}
	return client, nil
}

// devAuthorizer grants the "acme" tenant read/write on everything — paired with
// DevPrincipalFunc it authorizes a request that carries account-id: acme and
// denies one with no principal (fail closed).
func devAuthorizer() authz.Authorizer {
	return authz.NewDevAuthorizer(authz.Grant{
		Tenant: "acme", Subjects: []string{"*"}, Verbs: []authz.Verb{"*"}, Resource: "*",
	})
}

// Service is a running sample microservice: its gRPC address and a stop func.
type Service struct {
	Addr string
	stop func()
}

// Stop shuts the service down.
func (s *Service) Stop() { s.stop() }

// startServer constructs a fail-closed server on grpcAddr (":0" for an ephemeral
// port), runs register over it, and blocks until the gRPC listener binds.
// extraInterceptors are appended to the chain (the region service passes a spy
// that counts BatchGet + captures the read_mask for the e2e assertions).
func startServer(ctx context.Context, grpcAddr string, register func(*server.Server) error, extra ...grpc.UnaryServerInterceptor) (*Service, error) {
	if grpcAddr == "" {
		grpcAddr = ":0"
	}
	s, err := server.New(server.Config{
		GRPCAddr:      grpcAddr,
		Authorizer:    devAuthorizer(),
		PrincipalFunc: grpcauthz.DevPrincipalFunc(),
		Interceptors:  extra,
	})
	if err != nil {
		return nil, err
	}
	if err := register(s); err != nil {
		return nil, err
	}

	srvCtx, cancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() {
		if err := s.Serve(srvCtx); err != nil {
			serveErr <- err
		}
		close(serveErr)
	}()

	addr, err := waitForBind(s, serveErr)
	if err != nil {
		cancel()
		return nil, err
	}
	return &Service{Addr: addr, stop: cancel}, nil
}

// StartRegion starts the RegionService (the reference TARGET, serving the
// guaranteed BatchGetRegions) over its own ent+sqlite store on grpcAddr (":0"
// for an ephemeral port). extra interceptors (e.g. the e2e spy) are appended to
// the server chain.
func StartRegion(ctx context.Context, grpcAddr, dbName string, extra ...grpc.UnaryServerInterceptor) (*Service, error) {
	client, err := openEntClient(ctx, dbName)
	if err != nil {
		return nil, err
	}
	svc, err := startServer(ctx, grpcAddr, func(s *server.Server) error {
		return federationv1.RegisterRegionServiceWithRepository(s, federationv1.NewRegionEntBatchRepository(client))
	}, extra...)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	// Close the ent client when the service stops.
	stop := svc.stop
	svc.stop = func() { stop(); _ = client.Close() }
	return svc, nil
}

// registerAssetServiceNoRefGate wires the AssetService onto s exactly as the
// generated RegisterAssetServiceWithRepository does — gRPC handler, REST gateway,
// methods, authz rules — but WITHOUT s.RecordReferences. In the federated
// topology the referenced region BatchGet lives on a different process, so the
// server's same-server reference gate must not fire here (the gateway resolves
// the reference). This is the ONLY thing the sample changes relative to the
// generated one-call path.
func registerAssetServiceNoRefGate(s *server.Server, repo persistence.Repository[*federationv1.Asset, string]) error {
	s.RecordMethods(
		federationv1.AssetService_CreateAsset_FullMethodName,
		federationv1.AssetService_GetAsset_FullMethodName,
		federationv1.AssetService_ListAssets_FullMethodName,
	)
	s.AddRules(federationv1.AssetServiceAuthzRules...)
	federationv1.RegisterAssetServiceServer(s.GRPCServer(), federationv1.NewAssetServiceHandler(repo))
	s.RegisterGateway(func(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
		return federationv1.RegisterAssetServiceHandlerClient(ctx, mux, federationv1.NewAssetServiceClient(conn))
	})
	return nil
}

// StartAsset starts the AssetService (the reference SOURCE; Asset.region_id names
// a Region) over its own ent+sqlite store on grpcAddr (":0" for an ephemeral
// port).
//
// It registers the AssetService WITHOUT recording its cross-service reference on
// this server. That is the defining property of the federated (microservice-
// split) topology: the region service that serves BatchGetRegions is a SEPARATE
// process, so the server's same-server reference-target gate (which requires the
// referenced BatchGet to be co-located) does not apply here — the gateway is
// what resolves the reference across the two services. The generated
// AssetServiceReferences metadata still ships; the gateway consumes it. (The
// co-located variant, RegisterAssetServiceWithRepository, is what a single-binary
// deployment uses — see testdata/federation/server_test.go.)
func StartAsset(ctx context.Context, grpcAddr, dbName string, extra ...grpc.UnaryServerInterceptor) (*Service, error) {
	client, err := openEntClient(ctx, dbName)
	if err != nil {
		return nil, err
	}
	svc, err := startServer(ctx, grpcAddr, func(s *server.Server) error {
		return registerAssetServiceNoRefGate(s, federationv1.NewAssetEntRepository(client))
	}, extra...)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	stop := svc.stop
	svc.stop = func() { stop(); _ = client.Close() }
	return svc, nil
}
