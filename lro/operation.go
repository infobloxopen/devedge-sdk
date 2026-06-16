// Package lro implements the AIP-151 Long-Running Operation pattern.
// Operations are named "operations/{uuid}" and tracked in a [Store].
// Use [Manager.Submit] to start async work; use [WaitOperation] to poll
// until the operation completes.
package lro

import (
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when an operation name does not exist in the store.
var ErrNotFound = errors.New("lro: operation not found")

// ErrAlreadyDone is returned by [Manager.Cancel] when the operation has already
// completed (successfully or with an error) and therefore cannot be cancelled.
var ErrAlreadyDone = errors.New("lro: operation already done")

// ErrCancelled is set as [Operation.Err] when a caller cancels the operation
// before it completes.
var ErrCancelled = errors.New("lro: operation cancelled")

// Operation is an AIP-151 long-running operation resource.
// Name is always "operations/{uuid}". Done is true when the operation
// has completed, whether successfully (Response set) or with an error (Err set).
type Operation struct {
	Name       string
	Done       bool
	Metadata   any
	Response   any
	Err        error
	CreateTime time.Time
	UpdateTime time.Time
}

// OperationName returns the canonical resource name for an operation ID.
func OperationName(id string) string {
	return fmt.Sprintf("operations/%s", id)
}
