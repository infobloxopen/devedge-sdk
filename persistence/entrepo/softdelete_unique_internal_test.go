package entrepo

import (
	"testing"
	"time"

	"entgo.io/ent"
)

// fakeSoftDeleteMut implements the narrow softDeleteKeyMutation interface so the
// maintenance rule can be tested without a generated ent client.
type fakeSoftDeleteMut struct {
	op        ent.Op
	dtSet     bool // delete_time set in this mutation (soft-delete)
	dtCleared bool // ClearDeleteTime called (undelete)

	gotKey string
	keySet bool
}

func (f *fakeSoftDeleteMut) Op() ent.Op                    { return f.op }
func (f *fakeSoftDeleteMut) SetSoftDeleteKey(s string)     { f.gotKey = s; f.keySet = true }
func (f *fakeSoftDeleteMut) DeleteTime() (time.Time, bool) { return time.Time{}, f.dtSet }
func (f *fakeSoftDeleteMut) DeleteTimeCleared() bool       { return f.dtCleared }

func TestApplySoftDeleteKey(t *testing.T) {
	t.Run("soft-delete stamps a non-empty marker", func(t *testing.T) {
		m := &fakeSoftDeleteMut{op: ent.OpUpdateOne, dtSet: true}
		applySoftDeleteKey(m)
		if !m.keySet || m.gotKey == "" {
			t.Fatalf("soft-delete must stamp a non-empty soft_delete_key; keySet=%v key=%q", m.keySet, m.gotKey)
		}
	})

	t.Run("undelete clears the marker", func(t *testing.T) {
		m := &fakeSoftDeleteMut{op: ent.OpUpdateOne, dtCleared: true}
		applySoftDeleteKey(m)
		if !m.keySet || m.gotKey != "" {
			t.Fatalf("undelete must reset soft_delete_key to \"\"; keySet=%v key=%q", m.keySet, m.gotKey)
		}
	})

	t.Run("plain update leaves the marker untouched", func(t *testing.T) {
		m := &fakeSoftDeleteMut{op: ent.OpUpdate}
		applySoftDeleteKey(m)
		if m.keySet {
			t.Fatalf("a plain update must not touch soft_delete_key (got %q)", m.gotKey)
		}
	})

	t.Run("create leaves the marker at its default", func(t *testing.T) {
		m := &fakeSoftDeleteMut{op: ent.OpCreate, dtSet: true}
		applySoftDeleteKey(m)
		if m.keySet {
			t.Fatal("create must not stamp soft_delete_key (the field default \"\" marks live rows)")
		}
	})

	t.Run("two soft-deletes get distinct markers", func(t *testing.T) {
		a := &fakeSoftDeleteMut{op: ent.OpUpdateOne, dtSet: true}
		b := &fakeSoftDeleteMut{op: ent.OpUpdateOne, dtSet: true}
		applySoftDeleteKey(a)
		applySoftDeleteKey(b)
		if a.gotKey == b.gotKey {
			t.Fatalf("distinct soft-deletes must get distinct markers; both = %q", a.gotKey)
		}
	})
}
