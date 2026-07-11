package middleware

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
)

// DeduplicationStore is the backing store for DeduplicateUnary.
type DeduplicationStore interface {
	Load(requestID string) (any, bool)
	Store(requestID string, response any)
}

type dedupEntry struct {
	response  any
	expiresAt time.Time
}

// MemoryDeduplicationStore is a thread-safe in-memory DeduplicationStore with
// per-entry TTL.
type MemoryDeduplicationStore struct {
	mu  sync.Mutex
	m   map[string]dedupEntry
	ttl time.Duration
}

// NewMemoryDeduplicationStore returns a MemoryDeduplicationStore that expires
// entries after ttl.
func NewMemoryDeduplicationStore(ttl time.Duration) *MemoryDeduplicationStore {
	return &MemoryDeduplicationStore{
		m:   make(map[string]dedupEntry),
		ttl: ttl,
	}
}

func (s *MemoryDeduplicationStore) Load(requestID string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[requestID]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		delete(s.m, requestID)
		return nil, false
	}
	return e.response, true
}

func (s *MemoryDeduplicationStore) Store(requestID string, response any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[requestID] = dedupEntry{response: response, expiresAt: time.Now().Add(s.ttl)}
}

// requestIDGetter is implemented by generated mutation requests carrying an
// AIP-155 request_id.
type requestIDGetter interface {
	GetRequestId() string
}

// idempotencyCacheKey composes the deduplication cache key from the VERIFIED
// tenant, the gRPC method, and the client's request_id. Scoping by tenant closes
// the cross-tenant confidentiality leak that a bare request_id key allows (a
// request_id one tenant chose could otherwise replay another tenant's cached
// response on the same pod); scoping by method stops a request_id reused across
// operations from aliasing. The tenant comes from [VerifiedTenantID] (the
// verified principal), never the client-settable "account-id" header, so a
// spoofed header cannot widen or cross the scope. The NUL separators cannot
// appear in a method name and make the three parts unambiguous.
func idempotencyCacheKey(ctx context.Context, fullMethod, requestID string) string {
	tenant, _ := VerifiedTenantID(ctx)
	return tenant + "\x00" + fullMethod + "\x00" + requestID
}

// DeduplicateUnary returns a gRPC unary interceptor that deduplicates requests
// by request_id, SCOPED to the verified tenant and the method (see
// [idempotencyCacheKey]). Requests without a request_id or with
// validate_only=true pass through without touching the cache. Handler errors are
// not cached.
//
// This is the best-effort in-memory path: the cache is per-process and the store
// runs AFTER the handler returns, so it does not survive a crash/restart or a
// retry that lands on another pod, and it does not coalesce concurrent
// duplicates. For durable, exactly-once idempotency (a claim/complete inside the
// handler's transaction), use [DurableDeduplicateUnary].
func DeduplicateUnary(store DeduplicationStore) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		rg, ok := req.(requestIDGetter)
		if !ok {
			return handler(ctx, req)
		}
		requestID := rg.GetRequestId()
		if requestID == "" {
			return handler(ctx, req)
		}
		if ValidateOnlyFromContext(ctx) {
			return handler(ctx, req)
		}
		key := idempotencyCacheKey(ctx, info.FullMethod, requestID)
		if cached, hit := store.Load(key); hit {
			return cached, nil
		}
		resp, err := handler(ctx, req)
		if err != nil {
			return nil, err
		}
		store.Store(key, resp)
		return resp, nil
	}
}
