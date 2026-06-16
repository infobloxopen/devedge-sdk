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

// DeduplicateUnary returns a gRPC unary interceptor that deduplicates requests
// by request_id. Requests without a request_id or with validate_only=true pass
// through without touching the cache. Handler errors are not cached.
func DeduplicateUnary(store DeduplicationStore) grpc.UnaryServerInterceptor {
	type requestIDGetter interface {
		GetRequestId() string
	}
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
		if cached, hit := store.Load(requestID); hit {
			return cached, nil
		}
		resp, err := handler(ctx, req)
		if err != nil {
			return nil, err
		}
		store.Store(requestID, resp)
		return resp, nil
	}
}
