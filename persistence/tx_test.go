package persistence

import (
	"context"
	"errors"
	"testing"
)

func TestWithTx_TxFromContext_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := TxFromContext(ctx); ok {
		t.Fatal("a fresh context must carry no transaction")
	}

	handle := struct{ name string }{name: "tx-1"}
	ctx = WithTx(ctx, handle)
	got, ok := TxFromContext(ctx)
	if !ok {
		t.Fatal("TxFromContext must report the enrolled handle")
	}
	if got != handle {
		t.Fatalf("TxFromContext returned %v, want %v", got, handle)
	}
}

func TestRequireTx(t *testing.T) {
	if err := RequireTx(context.Background()); !errors.Is(err, ErrNoTransaction) {
		t.Fatalf("RequireTx without a tx: want ErrNoTransaction, got %v", err)
	}
	ctx := WithTx(context.Background(), struct{}{})
	if err := RequireTx(ctx); err != nil {
		t.Fatalf("RequireTx inside a tx: want nil, got %v", err)
	}
}
