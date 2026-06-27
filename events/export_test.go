package events

// export_test.go exposes unexported internals to the external (events_test) test
// package for white-box assertions.

// IdempotencyKeyForTest exposes the unexported idempotencyKey for the
// no-NUL-byte regression test (a NUL separator is unstorable on PostgreSQL).
func IdempotencyKeyForTest(eventID, handlerName string) string {
	return idempotencyKey(eventID, handlerName)
}
