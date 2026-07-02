package federationgql

import (
	"context"
	"sync"
)

// ctxKey is the private context-key type for the request-scoped values the
// gateway threads through GraphQL execution (the edge preload cache and the
// per-target read_mask).
type ctxKey int

const (
	cacheKey ctxKey = iota
	maskKey
)

// requestCache is the per-request store the eager preload (D-3) writes and the
// edge resolvers read: resolved targets keyed by (target type, id), the set of
// ids ALREADY batch-fetched per target type (so a second collection or a second
// reference to the same target only fetches the ids still missing — never a
// silent drop, never a needless refetch), and a per-target error so a failed
// preload (missing resolver, downstream denial) surfaces as a per-field GraphQL
// error rather than a silent null. It is mutation-safe because the graphql-go
// executor may resolve sibling fields concurrently.
type requestCache struct {
	mu      sync.Mutex
	targets map[string]map[string]any
	fetched map[string]map[string]struct{} // target type -> ids already BatchGot
	errs    map[string]error
}

func newRequestCache() *requestCache {
	return &requestCache{
		targets: map[string]map[string]any{},
		fetched: map[string]map[string]struct{}{},
		errs:    map[string]error{},
	}
}

// withCache returns ctx carrying c so resolvers can find it via cacheFrom.
func withCache(ctx context.Context, c *requestCache) context.Context {
	return context.WithValue(ctx, cacheKey, c)
}

// cacheFrom returns the request cache on ctx, or nil.
func cacheFrom(ctx context.Context) *requestCache {
	c, _ := ctx.Value(cacheKey).(*requestCache)
	return c
}

func (c *requestCache) put(targetType, id string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.targets[targetType]
	if m == nil {
		m = map[string]any{}
		c.targets[targetType] = m
	}
	m[id] = v
}

func (c *requestCache) get(targetType, id string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.targets[targetType]
	if m == nil {
		return nil, false
	}
	v, ok := m[id]
	return v, ok
}

// missingIDs returns the subset of ids for targetType that have NOT yet been
// batch-fetched in this request. Deduped and empty-filtered. The returned ids
// are the ONLY ones a preload needs to BatchGet — so N parents / a shared target
// still cost one BatchGet, and a second collection referencing the same target
// fetches only its own not-yet-seen ids.
func (c *requestCache) missingIDs(targetType string, ids []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	got := c.fetched[targetType]
	seen := map[string]struct{}{}
	var out []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if got != nil {
			if _, already := got[id]; already {
				continue
			}
		}
		out = append(out, id)
	}
	return out
}

// markFetched records that ids have been batch-fetched for targetType, so a
// later preload for the same target does not refetch them.
func (c *requestCache) markFetched(targetType string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	got := c.fetched[targetType]
	if got == nil {
		got = map[string]struct{}{}
		c.fetched[targetType] = got
	}
	for _, id := range ids {
		got[id] = struct{}{}
	}
}

func (c *requestCache) setErr(targetType string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs[targetType] = err
}

func (c *requestCache) errFor(targetType string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errs[targetType]
}

// readMaskCtx carries the per-target read_mask a MaskAwareBatchGetter reads.
type readMaskCtx map[string][]string

// withReadMask stashes mask for targetType on ctx (additive across calls).
func withReadMask(ctx context.Context, targetType string, mask []string) context.Context {
	existing, _ := ctx.Value(maskKey).(readMaskCtx)
	next := readMaskCtx{}
	for k, v := range existing {
		next[k] = v
	}
	next[targetType] = mask
	return context.WithValue(ctx, maskKey, next)
}

// ReadMaskFromContext returns the read_mask paths the gateway derived for
// targetType from the GraphQL selection set (D-5), or nil. A resolver's
// downstream client that implements masking reads it here to push the mask
// down (see [MaskAwareBatchGetter]).
func ReadMaskFromContext(ctx context.Context, targetType string) []string {
	m, _ := ctx.Value(maskKey).(readMaskCtx)
	if m == nil {
		return nil
	}
	return m[targetType]
}
