package cells_test

import (
	"testing"

	"github.com/infobloxopen/devedge-sdk/cells"
)

func TestState_AdmitsNew(t *testing.T) {
	cases := []struct {
		state cells.State
		want  bool
	}{
		{cells.StateActive, true},
		{cells.StateActiveNew, true},
		{cells.StateAborted, true},
		{cells.StateUnknown, true},
		{cells.StateQuiescing, false},
		{cells.StateDraining, false},
		{cells.StateCopying, false},
		{cells.StateCommitting, false},
	}
	for _, tc := range cases {
		if got := tc.state.AdmitsNew(); got != tc.want {
			t.Errorf("State(%v).AdmitsNew() = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestState_IsMoving(t *testing.T) {
	cases := []struct {
		state cells.State
		want  bool
	}{
		{cells.StateQuiescing, true},
		{cells.StateDraining, true},
		{cells.StateCopying, true},
		{cells.StateCommitting, true},
		{cells.StateActive, false},
		{cells.StateActiveNew, false},
		{cells.StateAborted, false},
		{cells.StateUnknown, false},
	}
	for _, tc := range cases {
		if got := tc.state.IsMoving(); got != tc.want {
			t.Errorf("State(%v).IsMoving() = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestState_String(t *testing.T) {
	cases := []struct {
		state cells.State
		want  string
	}{
		{cells.StateActive, "ACTIVE"},
		{cells.StateQuiescing, "QUIESCING"},
		{cells.StateDraining, "DRAINING"},
		{cells.StateCopying, "COPYING"},
		{cells.StateCommitting, "COMMITTING"},
		{cells.StateActiveNew, "ACTIVE_NEW"},
		{cells.StateAborted, "ABORTED"},
		{cells.StateUnknown, "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestTenantRoute_IsZero(t *testing.T) {
	var zero cells.TenantRoute
	if !zero.IsZero() {
		t.Error("zero TenantRoute must report IsZero() == true")
	}

	nonZero := cells.TenantRoute{TenantID: "t1"}
	if nonZero.IsZero() {
		t.Error("TenantRoute with TenantID set must report IsZero() == false")
	}

	withEpoch := cells.TenantRoute{RouteEpoch: 1}
	if withEpoch.IsZero() {
		t.Error("TenantRoute with RouteEpoch>0 must report IsZero() == false")
	}

	withState := cells.TenantRoute{State: cells.StateActive}
	if withState.IsZero() {
		t.Error("TenantRoute with State!=StateUnknown must report IsZero() == false")
	}
}

// AdmitsNew and IsMoving are mutually exclusive across the moving states.
func TestState_AdmitsNew_IsMoving_Exclusive(t *testing.T) {
	all := []cells.State{
		cells.StateUnknown,
		cells.StateActive,
		cells.StateQuiescing,
		cells.StateDraining,
		cells.StateCopying,
		cells.StateCommitting,
		cells.StateActiveNew,
		cells.StateAborted,
	}
	for _, s := range all {
		if s.IsMoving() && s.AdmitsNew() {
			t.Errorf("State %v: IsMoving()=true and AdmitsNew()=true simultaneously — must be mutually exclusive", s)
		}
	}
}
