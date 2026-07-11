package persistence

// idempotency.go — the neutral data types for the durable, exactly-once
// request-idempotency seam (WS-043 / F048). They live in the clean core (not the
// gRPC middleware layer) so a persistence adapter can implement the store without
// importing middleware, keeping the dependency direction adapter → core. The
// consuming interceptor (middleware.DurableDeduplicateUnary) and the store
// interface (middleware.DurableIdempotencyStore) reference these types.

// IdempotencyStatus is the lifecycle state of a durable idempotency record.
type IdempotencyStatus string

const (
	// StatusInProgress marks a claimed-but-not-yet-completed request. A concurrent
	// duplicate that observes it is a conflict, not a second execution.
	StatusInProgress IdempotencyStatus = "in_progress"
	// StatusCompleted marks a finished request whose response is stored for replay.
	StatusCompleted IdempotencyStatus = "completed"
)

// IdempotencyKey is the tenant-scoped, per-operation identity of a request. It is
// NEVER the bare request_id: Tenant fences one tenant's keys from another's and
// Method stops a request_id reused across operations from aliasing. Tenant is the
// verified principal's tenant; it may be empty for system/unauthenticated paths,
// which then share one scope.
type IdempotencyKey struct {
	Tenant    string // account_id — the confidentiality/RLS scope
	Method    string // gRPC full method
	RequestID string // AIP-155 request_id
}

// IdempotencyRecord is the persisted state of one idempotency key.
type IdempotencyRecord struct {
	Status IdempotencyStatus
	// ResponseType is the full proto message name of Response (completed only).
	ResponseType string
	// Response is the marshaled proto response (completed only).
	Response []byte
	// Fingerprint is the hex SHA-256 of the deterministically-marshaled request
	// when fingerprinting is enabled, else empty.
	Fingerprint string
}
