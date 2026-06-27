package persistence

import (
	"context"
	"errors"
	"reflect"
	"testing"

	dddpb "github.com/infobloxopen/devedge-sdk/proto/infoblox/ddd/v1"
	"google.golang.org/protobuf/proto"
)

// --- Round-3 hardening validation of the F030 deep-copy (cloneEntity/deepCopyValue).
//
// These tests target the cases the round-2 regression suite did NOT cover directly:
//   (b) aliasing preservation across a cyclic / shared-pointer graph,
//   (c) a REAL proto message round-trip (unexported state/sizeCache/unknownFields),
//   (d) no panic on nil pointers, nil maps/slices, arrays, interfaces, and
//       unexported struct fields.

// aliased holds two pointer fields that, in the source, point at the SAME object.
// A structurally faithful deep copy must keep them aliased in the copy too (both
// fields point at the ONE copied object), not split them into two independent
// copies — otherwise mutating through one field would not be seen through the
// other, silently changing program semantics across a snapshot/restore.
type aliased struct {
	ID string
	A  *aliasInner
	B  *aliasInner
}

type aliasInner struct{ N int }

// TestDeepCopy_PreservesAliasing asserts the visited-pointer guard re-aliases a
// shared pointer rather than copying it twice. After the copy, cp.A and cp.B must
// be the SAME (copied) pointer, and that pointer must NOT be the source pointer.
func TestDeepCopy_PreservesAliasing(t *testing.T) {
	shared := &aliasInner{N: 1}
	src := &aliased{ID: "x", A: shared, B: shared}

	cp := cloneEntity(src)

	if cp == src {
		t.Fatal("top-level pointer was not copied (cp == src)")
	}
	if cp.A == nil || cp.B == nil {
		t.Fatal("copied alias fields must not be nil")
	}
	if cp.A != cp.B {
		t.Errorf("aliasing not preserved: cp.A (%p) and cp.B (%p) should be the same object", cp.A, cp.B)
	}
	if cp.A == src.A {
		t.Error("copied alias object must be isolated from the source (got the source pointer)")
	}
	// And it must be isolated: mutating the copy does not touch the source, and the
	// copy stays internally consistent (one mutation seen through both fields).
	cp.A.N = 99
	if cp.B.N != 99 {
		t.Errorf("alias broken: mutation via cp.A not seen via cp.B (cp.B.N=%d)", cp.B.N)
	}
	if src.A.N != 1 {
		t.Errorf("source leaked: src.A.N=%d want 1", src.A.N)
	}
}

// TestDeepCopy_PreservesAliasingThroughSlice covers the same property when the
// shared object is reached through a slice (the aggregate `Items []*item` shape):
// two slice elements pointing at one object stay aliased after the copy.
func TestDeepCopy_PreservesAliasingThroughSlice(t *testing.T) {
	shared := &aliasInner{N: 7}
	type holder struct {
		ID    string
		Items []*aliasInner
	}
	src := &holder{ID: "h", Items: []*aliasInner{shared, shared}}

	cp := cloneEntity(src)
	if cp.Items[0] != cp.Items[1] {
		t.Errorf("slice aliasing not preserved: Items[0] (%p) != Items[1] (%p)", cp.Items[0], cp.Items[1])
	}
	if cp.Items[0] == src.Items[0] {
		t.Error("copied slice element must be isolated from source")
	}
}

// TestDeepCopy_ProtoMember round-trips a REAL generated proto message through a
// snapshot+rollback and a snapshot+commit. The proto carries the unexported
// protoimpl state/sizeCache/unknownFields; the deep copy value-copies them at the
// struct level and must NOT corrupt the message: it stays a valid, marshalable,
// proto.Equal-equivalent message, and rollback truly reverts a mutated field.
func TestDeepCopy_ProtoMember(t *testing.T) {
	ctx := context.Background()
	// Key by a STABLE id (not the mutated field) so an in-place field mutation does
	// not change the entity's map key.
	const id = "m1"
	r := NewMemoryRepository(func(m *dddpb.Member) string { return id })
	tx := NewMemoryTxRunner(r)

	orig := &dddpb.Member{Root: "Order"}
	// Force the proto runtime to populate internal state (mirrors a marshaled message).
	_, _ = proto.Marshal(orig)
	if _, err := r.Create(ctx, orig); err != nil {
		t.Fatal(err)
	}

	// Rollback: mutate the proto field through a Get-returned message, then discard.
	boom := errors.New("rollback")
	_ = tx.Atomically(ctx, func(txCtx context.Context) error {
		m, err := r.Get(txCtx, id)
		if err != nil {
			return err
		}
		m.Root = "MUTATED"
		return boom
	})
	got, err := r.Get(ctx, id)
	if err != nil {
		t.Fatalf("proto member must survive rollback: %v", err)
	}
	if got.GetRoot() != "Order" {
		t.Errorf("proto mutation leaked across rollback: Root=%q want Order", got.GetRoot())
	}
	// The restored message is still a healthy proto: marshalable and Equal to a fresh one.
	if _, err := proto.Marshal(got); err != nil {
		t.Fatalf("restored proto is corrupted (marshal failed): %v", err)
	}
	if !proto.Equal(got, &dddpb.Member{Root: "Order"}) {
		t.Errorf("restored proto not Equal to a fresh equivalent: %v", got)
	}

	// Commit: a mutation that commits is observed afterward and still marshalable.
	if err := tx.Atomically(ctx, func(txCtx context.Context) error {
		m, _ := r.Get(txCtx, id)
		m.Root = "Order2"
		return nil
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got2, err := r.Get(ctx, id)
	if err != nil {
		t.Fatalf("committed proto mutation lost: %v", err)
	}
	if got2.GetRoot() != "Order2" {
		t.Errorf("committed proto mutation: Root=%q want Order2", got2.GetRoot())
	}
	if _, err := proto.Marshal(got2); err != nil {
		t.Fatalf("committed proto is corrupted (marshal failed): %v", err)
	}
}

// edgey exercises the panic-safety surface of deepCopyValue in one struct:
// a nil pointer, a nil slice, a nil map, a fixed array, an interface field
// (both nil and non-nil), and unexported fields (mixed with exported).
type edgey struct {
	ID       string
	NilPtr   *aliasInner
	NilSlice []string
	NilMap   map[string]int
	Arr      [3]int
	Iface    any
	IfaceNil any
	priv     int        // unexported scalar
	privPtr  *aliasInner // unexported pointer
}

// TestDeepCopy_EdgeCasesNoPanic asserts deepCopyValue handles every awkward field
// kind without panicking and isolates the settable (exported) ones. Unexported
// fields are value-copied (shared reference is acceptable per the documented
// contract) but must not cause a panic.
func TestDeepCopy_EdgeCasesNoPanic(t *testing.T) {
	src := &edgey{
		ID:    "e",
		Arr:   [3]int{1, 2, 3},
		Iface: &aliasInner{N: 5},
		priv:  42,
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("deepCopyValue panicked on edge-case struct: %v", r)
		}
	}()

	var cp *edgey
	func() {
		cp = cloneEntity(src)
	}()

	if cp == src {
		t.Fatal("top-level pointer not copied")
	}
	if cp.NilPtr != nil || cp.NilSlice != nil || cp.NilMap != nil {
		t.Error("nil reference fields must stay nil after copy")
	}
	if cp.Arr != [3]int{1, 2, 3} {
		t.Errorf("array not copied faithfully: %v", cp.Arr)
	}
	if cp.IfaceNil != nil {
		t.Error("nil interface field must stay nil")
	}
	// The non-nil interface holding a *aliasInner is deep-copied and isolated.
	cpInner, ok := cp.Iface.(*aliasInner)
	if !ok || cpInner == nil {
		t.Fatalf("interface field lost its concrete value: %#v", cp.Iface)
	}
	if cpInner == src.Iface.(*aliasInner) {
		t.Error("interface-held pointer must be isolated from source")
	}
	cpInner.N = 123
	if src.Iface.(*aliasInner).N != 5 {
		t.Error("interface deep-copy leaked into source")
	}
	// Unexported scalar value-copied through.
	if cp.priv != 42 {
		t.Errorf("unexported scalar not preserved: %d", cp.priv)
	}
}

// TestDeepCopy_NilSliceVsEmptySlice ensures the nil-vs-empty distinction is
// preserved (a nil slice stays nil; an empty non-nil slice stays non-nil). A naive
// MakeSlice on a nil source would wrongly turn nil into [].
func TestDeepCopy_NilSliceVsEmptySlice(t *testing.T) {
	type s struct {
		ID    string
		NilSl []int
		Empty []int
	}
	src := &s{ID: "s", NilSl: nil, Empty: []int{}}
	cp := cloneEntity(src)
	if cp.NilSl != nil {
		t.Error("nil slice must stay nil")
	}
	if cp.Empty == nil {
		t.Error("empty non-nil slice must stay non-nil")
	}
}

// TestDeepCopy_DirectCloneEntityNilPointer guards the top-level nil-pointer path
// (a nil *T) — must return nil without panicking.
func TestDeepCopy_DirectCloneEntityNilPointer(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("cloneEntity(nil pointer) panicked: %v", r)
		}
	}()
	var p *aliasInner
	cp := cloneEntity(p)
	if cp != nil {
		t.Errorf("cloneEntity(nil) = %v want nil", cp)
	}
}

// TestDeepCopy_ValueTypeUnchanged confirms a non-pointer/slice/map T returns
// unchanged (the documented historical contract — value semantics already isolate
// the top level), and that this does not panic on an interface-typed reflect.Value.
func TestDeepCopy_ValueTypeUnchanged(t *testing.T) {
	got := cloneEntity(struct{ N int }{N: 7})
	if got.N != 7 {
		t.Errorf("value-type clone lost data: %v", got)
	}
	// Sanity: deepCopyValue on an interface-kind reflect.Value falls through to a
	// plain copy without panicking.
	var iface any = &aliasInner{N: 1}
	rv := reflect.ValueOf(&iface).Elem() // a settable interface-kind value
	_ = deepCopyValue(rv, map[uintptr]reflect.Value{})
}
