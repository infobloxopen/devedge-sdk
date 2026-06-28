package entrepo

import (
	"context"
	"errors"
	"testing"

	"entgo.io/ent"
)

// fakeTenantGuardMut implements the narrow tenantGuardMutation interface so the
// cross-tenant write rule can be tested without a generated ent client.
type fakeTenantGuardMut struct {
	op     ent.Op
	oldVal ent.Value
	oldErr error
}

func (f *fakeTenantGuardMut) Op() ent.Op { return f.op }
func (f *fakeTenantGuardMut) OldField(_ context.Context, name string) (ent.Value, error) {
	if name != "account_id" {
		return nil, errors.New("unexpected field")
	}
	return f.oldVal, f.oldErr
}

func TestCheckTenantWrite(t *testing.T) {
	ctx := context.Background()

	t.Run("no ctx tenant → allow", func(t *testing.T) {
		m := &fakeTenantGuardMut{op: ent.OpUpdateOne, oldVal: "other"}
		if err := checkTenantWrite(ctx, m, ""); err != nil {
			t.Fatalf("no-tenant request must be allowed, got %v", err)
		}
	})

	t.Run("create is not guarded", func(t *testing.T) {
		m := &fakeTenantGuardMut{op: ent.OpCreate}
		if err := checkTenantWrite(ctx, m, "acme"); err != nil {
			t.Fatalf("create must not be guarded, got %v", err)
		}
	})

	t.Run("update of OWN row → allow", func(t *testing.T) {
		m := &fakeTenantGuardMut{op: ent.OpUpdateOne, oldVal: "acme"}
		if err := checkTenantWrite(ctx, m, "acme"); err != nil {
			t.Fatalf("update of own row must be allowed, got %v", err)
		}
	})

	t.Run("update of ANOTHER tenant's row → reject", func(t *testing.T) {
		m := &fakeTenantGuardMut{op: ent.OpUpdateOne, oldVal: "victim"}
		if err := checkTenantWrite(ctx, m, "attacker"); !errors.Is(err, ErrCrossTenantWrite) {
			t.Fatalf("cross-tenant update must be ErrCrossTenantWrite, got %v", err)
		}
	})

	t.Run("delete of ANOTHER tenant's row → reject", func(t *testing.T) {
		m := &fakeTenantGuardMut{op: ent.OpDeleteOne, oldVal: "victim"}
		if err := checkTenantWrite(ctx, m, "attacker"); !errors.Is(err, ErrCrossTenantWrite) {
			t.Fatalf("cross-tenant delete must be ErrCrossTenantWrite, got %v", err)
		}
	})

	t.Run("batch op (OldField errors) → allow (interceptor scopes the read path)", func(t *testing.T) {
		m := &fakeTenantGuardMut{op: ent.OpUpdate, oldErr: errors.New("OldField is allowed only on UpdateOne operations")}
		if err := checkTenantWrite(ctx, m, "acme"); err != nil {
			t.Fatalf("batch op must fall through (allowed), got %v", err)
		}
	})

	t.Run("empty old account_id → allow", func(t *testing.T) {
		m := &fakeTenantGuardMut{op: ent.OpUpdateOne, oldVal: ""}
		if err := checkTenantWrite(ctx, m, "acme"); err != nil {
			t.Fatalf("empty old account_id must fall through (allowed), got %v", err)
		}
	})
}
