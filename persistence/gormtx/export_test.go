package gormtx

import "time"

// WithOutboxNowForTest exposes the unexported clock-override option to the
// external _test package so the outbox lease tests can drive lease expiry
// deterministically. It lives in a _test.go file, so it never ships in the real
// package — the public API keeps no test-only knob.
func WithOutboxNowForTest(now func() time.Time) OutboxOption { return withOutboxNow(now) }
