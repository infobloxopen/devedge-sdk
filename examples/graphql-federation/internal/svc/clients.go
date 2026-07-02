package svc

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/infobloxopen/devedge-sdk/federationgql"
	"github.com/infobloxopen/devedge-sdk/testdata/federation/federationv1"
)

// ReadMaskMDKey is the gRPC metadata key the gateway uses to push the AIP-157
// read_mask down to the region service's BatchGet (the fixture's
// BatchGetRegionsRequest has no read_mask field, so the mask travels as metadata
// — a real pushdown the region service observes and honors). Comma-separated
// field paths.
const ReadMaskMDKey = "read-mask"

// regionBatchClient is the gateway's downstream client for the region reference
// target. It is a federationgql.MaskAwareBatchGetter: its BatchGet reads the
// read_mask the gateway derived from the GraphQL selection set
// (federationgql.ReadMaskFromContext) and pushes it down as gRPC metadata (D-5),
// then calls the guaranteed AIP-137 BatchGetRegions — so N asset references
// resolve in exactly ONE call (D-3 / AC-3).
type regionBatchClient struct {
	client federationv1.RegionServiceClient
}

// BatchGet implements reference.BatchGetter[*federationv1.Region]. The incoming
// ctx already carries the caller's identity (account-id) forwarded by the
// gateway; this method additionally stamps the derived read_mask as metadata.
func (c *regionBatchClient) BatchGet(ctx context.Context, ids []string) ([]*federationv1.Region, error) {
	// Forward the caller's identity as gRPC metadata (account-id) so the region
	// service's fail-closed interceptor authorizes the batch read — the gateway
	// propagates, never elevates (D-4).
	ctx = outgoingCtx(ctx)
	if mask := federationgql.ReadMaskFromContext(ctx, "region.example.com/Region"); len(mask) > 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, ReadMaskMDKey, strings.Join(mask, ","))
	}
	resp, err := c.client.BatchGetRegions(ctx, &federationv1.BatchGetRegionsRequest{Ids: ids})
	if err != nil {
		return nil, err
	}
	return resp.GetRegions(), nil
}

// NewRegionBatchClient wraps a dialed RegionService gRPC connection as the
// gateway's mask-aware BatchGet client.
func NewRegionBatchClient(conn grpc.ClientConnInterface) *regionBatchClient {
	return &regionBatchClient{client: federationv1.NewRegionServiceClient(conn)}
}

// assetClient is the gateway's downstream client for the AssetService (the
// reference SOURCE root query). It forwards the execution context (identity) to
// ListAssets / GetAsset and pushes the read_mask down on the fixture's native
// read_mask field (AIP-157).
type assetClient struct {
	client federationv1.AssetServiceClient
}

// NewAssetClient wraps a dialed AssetService gRPC connection.
func NewAssetClient(conn grpc.ClientConnInterface) *assetClient {
	return &assetClient{client: federationv1.NewAssetServiceClient(conn)}
}

// ReadMaskFromIncoming returns the read_mask the gateway pushed down as metadata,
// read server-side from the incoming gRPC context (the e2e spy asserts on it to
// prove AC-4). Empty when the caller sent no mask.
func ReadMaskFromIncoming(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(ReadMaskMDKey); len(v) > 0 {
		return v[0]
	}
	return ""
}
