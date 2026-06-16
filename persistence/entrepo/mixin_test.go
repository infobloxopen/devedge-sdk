package entrepo_test

import (
	"testing"

	"entgo.io/ent"

	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
)

func TestTenantMixin_HasAccountIDField(t *testing.T) {
	m := entrepo.TenantMixin{}
	fields := m.Fields()
	found := false
	for _, f := range fields {
		// field.String("account_id") — check by descriptor
		if f.Descriptor().Name == "account_id" {
			found = true
		}
	}
	if !found {
		t.Fatal("TenantMixin.Fields() must include account_id")
	}
}

func TestTenantMixin_HasInterceptor(t *testing.T) {
	m := entrepo.TenantMixin{}
	if len(m.Interceptors()) == 0 {
		t.Fatal("TenantMixin must have at least one interceptor")
	}
}

// Compile-time check that TenantMixin implements ent.Mixin
var _ ent.Mixin = entrepo.TenantMixin{}

// F020: SoftDeleteMixin tests.

func TestSoftDeleteMixin_HasDeleteTimeField(t *testing.T) {
	m := entrepo.SoftDeleteMixin{}
	fields := m.Fields()
	found := false
	for _, f := range fields {
		if f.Descriptor().Name == "delete_time" {
			found = true
			// Must be optional/nillable.
			if !f.Descriptor().Optional {
				t.Error("SoftDeleteMixin delete_time field must be optional")
			}
			if !f.Descriptor().Nillable {
				t.Error("SoftDeleteMixin delete_time field must be nillable")
			}
		}
	}
	if !found {
		t.Fatal("SoftDeleteMixin.Fields() must include delete_time")
	}
}

func TestSoftDeleteMixin_HasInterceptor(t *testing.T) {
	m := entrepo.SoftDeleteMixin{}
	if len(m.Interceptors()) == 0 {
		t.Fatal("SoftDeleteMixin must have at least one interceptor")
	}
}

// Compile-time check that SoftDeleteMixin implements ent.Mixin.
var _ ent.Mixin = entrepo.SoftDeleteMixin{}
