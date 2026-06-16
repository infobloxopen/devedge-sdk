package lro

import (
	"context"
	"time"
)

// WaitOperation polls store until the named operation is done, ctx is cancelled,
// or ctx's deadline is exceeded. It returns the final operation on success.
// poll is the interval between polls; a zero poll defaults to 100ms.
func WaitOperation(ctx context.Context, store Store, name string, poll time.Duration) (*Operation, error) {
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	for {
		op, err := store.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if op.Done {
			return op, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}
