package federationgql

import (
	"context"

	"github.com/infobloxopen/devedge-sdk/reference"
)

// The gateway resolves reference edges through reference.Load, which is generic
// over the target's Go type T and type-asserts the resolver's registered client
// to a reference.BatchGetter[T]. The gateway is type-erased (it composes many
// resource types behind one schema), so it calls reference.Load[any, any] and
// therefore needs each registered client to be a reference.BatchGetter[any].
//
// AnyGetter adapts a typed reference.BatchGetter[T] (what a service's generated
// BatchGet client naturally is — e.g. BatchGetter[*regionv1.Region]) into a
// reference.BatchGetter[any] the gateway's resolver can register. Register the
// result under the reference's TargetType in the reference.ReferenceResolver you
// pass to [NewSchema].
//
//	resolver := reference.NewStaticResolver()
//	resolver.Register("region.example.com/Region",
//	    federationgql.AnyGetter[*regionv1.Region](regionBatchClient))
//
// A client that pushes AIP-157 read_mask down (D-5) reads the mask inside its
// own BatchGet via [ReadMaskFromContext]; AnyGetter forwards the context
// unchanged, so masking composes.
func AnyGetter[T any](g reference.BatchGetter[T]) reference.BatchGetter[any] {
	return anyGetter[T]{g: g}
}

type anyGetter[T any] struct {
	g reference.BatchGetter[T]
}

func (a anyGetter[T]) BatchGet(ctx context.Context, ids []string) ([]any, error) {
	ts, err := a.g.BatchGet(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(ts))
	for i, t := range ts {
		out[i] = t
	}
	return out, nil
}
