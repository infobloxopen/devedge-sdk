package svc

import (
	"context"
	"fmt"
	"net/http"

	"github.com/graphql-go/graphql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/infobloxopen/devedge-sdk/federationgql"
	"github.com/infobloxopen/devedge-sdk/reference"
	"github.com/infobloxopen/devedge-sdk/testdata/federation/federationv1"
)

// identityKey carries the caller's account-id from the HTTP edge into the
// GraphQL execution context so the downstream gRPC clients forward it as
// metadata. The gateway makes ZERO authz decisions — it only propagates
// identity (D-4). A request with no identity forwards none, and each service's
// fail-closed interceptor denies it (AC-5).
type identityKey struct{}

// WithAccountID stamps the caller's account-id on ctx (used by the HTTP edge
// middleware and the e2e test).
func WithAccountID(ctx context.Context, accountID string) context.Context {
	if accountID == "" {
		return ctx
	}
	return context.WithValue(ctx, identityKey{}, accountID)
}

// outgoingCtx converts the propagated identity on ctx into gRPC outgoing
// metadata (account-id) for a downstream call. No identity -> no metadata ->
// the downstream service denies (fail closed).
func outgoingCtx(ctx context.Context) context.Context {
	if acct, ok := ctx.Value(identityKey{}).(string); ok && acct != "" {
		return metadata.AppendToOutgoingContext(ctx, "account-id", acct)
	}
	return ctx
}

// Gateway holds the assembled GraphQL schema + the downstream connections.
type Gateway struct {
	Schema graphql.Schema
	conns  []*grpc.ClientConn
}

// Close closes the downstream gRPC connections.
func (g *Gateway) Close() {
	for _, c := range g.conns {
		_ = c.Close()
	}
}

// Handler returns the GraphQL HTTP handler, wrapped so the caller's X-Account-Id
// header is propagated into the execution context (and thence to the downstream
// services as gRPC metadata). This is the gateway's only "identity" step — it
// trusts the header for the sample exactly as the services trust DevPrincipalFunc
// metadata; production would place a verified principal here instead.
func (g *Gateway) Handler() http.Handler {
	inner := federationgql.Handler(g.Schema)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithAccountID(r.Context(), r.Header.Get("X-Account-Id"))
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
}

// NewGateway dials the region + asset services and builds the federation schema:
// Asset (root assets/asset) with a region edge -> Region (root regions/region),
// resolved through the guaranteed BatchGetRegions in exactly one call per
// collection (D-3). regionAddr / assetAddr are the services' gRPC addresses.
func NewGateway(regionAddr, assetAddr string) (*Gateway, error) {
	regionConn, err := grpc.NewClient(regionAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial region: %w", err)
	}
	assetConn, err := grpc.NewClient(assetAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = regionConn.Close()
		return nil, fmt.Errorf("dial asset: %w", err)
	}

	region := NewRegionBatchClient(regionConn)
	asset := NewAssetClient(assetConn)

	// The resolver maps the reference target type -> the region BatchGet client,
	// adapted to the type-erased BatchGetter[any] the gateway resolves through.
	resolver := reference.NewStaticResolver()
	resolver.Register("region.example.com/Region", federationgql.AnyGetter[*federationv1.Region](region))

	schema, err := federationgql.NewSchema(
		[]federationgql.Resource{assetDescriptor(asset), regionDescriptor()},
		resolver,
	)
	if err != nil {
		_ = regionConn.Close()
		_ = assetConn.Close()
		return nil, fmt.Errorf("build schema: %w", err)
	}
	return &Gateway{Schema: schema, conns: []*grpc.ClientConn{regionConn, assetConn}}, nil
}

// regionDescriptor maps Region to its GraphQL type. Region is a pure reference
// target here (resolved via the edge), so it needs no root Get/List for the
// sample — but exposing them makes `regions`/`region(id)` queryable too. The
// gateway holds no region client for the root here; the sample's root queries
// start at assets, so Region's Get/List are omitted (nil) and the type is
// reached only through the edge.
func regionDescriptor() federationgql.Resource {
	return federationgql.Resource{
		Type: "region.example.com/Region",
		Name: "Region",
		Scalars: []federationgql.ScalarField{
			{Name: "id", MaskPath: "id", Resolve: func(s any) any { return s.(*federationv1.Region).GetId() }},
			{Name: "name", MaskPath: "display_name", Resolve: func(s any) any { return s.(*federationv1.Region).GetDisplayName() }},
		},
		IDOf: func(s any) string { return s.(*federationv1.Region).GetId() },
	}
}

// assetDescriptor maps Asset to its GraphQL type, with the region edge derived
// from the generated AssetServiceReferences metadata and root list/get backed by
// the dialed AssetService client (identity propagated, read_mask pushed down on
// the native field).
func assetDescriptor(c *assetClient) federationgql.Resource {
	return federationgql.Resource{
		Type: "asset.example.com/Asset",
		Name: "Asset",
		Scalars: []federationgql.ScalarField{
			{Name: "id", MaskPath: "id", Resolve: func(s any) any { return s.(*federationv1.Asset).GetId() }},
			{Name: "name", MaskPath: "display_name", Resolve: func(s any) any { return s.(*federationv1.Asset).GetDisplayName() }},
			{Name: "regionId", MaskPath: "region_id", Resolve: func(s any) any { return s.(*federationv1.Asset).GetRegionId() }},
		},
		// The edge comes straight from the generated <Svc>References table.
		References: federationv1.AssetServiceReferences,
		List: func(ctx context.Context, args federationgql.ListArgs) ([]any, error) {
			pageSize := args.PageSize
			if pageSize < 0 {
				pageSize = 0 // let the service apply its default rather than send a negative
			}
			resp, err := c.client.ListAssets(outgoingCtx(ctx), &federationv1.ListAssetsRequest{
				PageSize:  int32(pageSize),
				PageToken: args.PageToken,
			})
			if err != nil {
				return nil, err
			}
			out := make([]any, len(resp.GetAssets()))
			for i, a := range resp.GetAssets() {
				out[i] = a
			}
			return out, nil
		},
		Get: func(ctx context.Context, args federationgql.GetArgs) (any, error) {
			a, err := c.client.GetAsset(outgoingCtx(ctx), &federationv1.GetAssetRequest{Id: args.ID})
			if err != nil {
				return nil, err
			}
			return a, nil
		},
		IDOf:   func(s any) string { return s.(*federationv1.Asset).GetId() },
		RefIDs: func(_ reference.Reference, s any) []string { return []string{s.(*federationv1.Asset).GetRegionId()} },
	}
}
