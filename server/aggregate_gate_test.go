package server

import (
	"strings"
	"testing"
)

// TestAssertAggregateBoundaries is F031 AC-1: a member resource that registers a
// write-capable standard method fails the boundary gate; removing/redirecting the
// write serves; Get/List for the member are unaffected (reads ≠ write authority).
func TestAssertAggregateBoundaries(t *testing.T) {
	const (
		createItem = "/order.v1.ItemService/CreateItem"
		updateItem = "/order.v1.ItemService/UpdateItem"
		getItem    = "/order.v1.ItemService/GetItem"
		listItems  = "/order.v1.ItemService/ListItems"
	)

	// Violation: Item is a member of Order and registers CreateItem (a write).
	t.Run("member write registered → fails", func(t *testing.T) {
		methods := []string{createItem, getItem, listItems}
		members := []MemberBinding{{
			Resource:     "Item",
			Root:         "Order",
			WriteMethods: []string{createItem, updateItem},
		}}
		err := AssertAggregateBoundaries(methods, members)
		if err == nil {
			t.Fatal("a registered member write must fail the boundary gate")
		}
		if !strings.Contains(err.Error(), "Item") || !strings.Contains(err.Error(), "Order") || !strings.Contains(err.Error(), createItem) {
			t.Fatalf("error should name the member, root, and method: %v", err)
		}
	})

	// Redirected/removed: the member registers only Get/List → serves.
	t.Run("member with only reads → serves", func(t *testing.T) {
		methods := []string{getItem, listItems}
		members := []MemberBinding{{
			Resource:     "Item",
			Root:         "Order",
			WriteMethods: []string{createItem, updateItem}, // declared, but not registered
		}}
		if err := AssertAggregateBoundaries(methods, members); err != nil {
			t.Fatalf("a member exposing only Get/List must serve, got %v", err)
		}
	})

	// No member bindings at all (non-aggregate service) → serves.
	t.Run("no members → serves", func(t *testing.T) {
		methods := []string{createItem, updateItem, getItem, listItems}
		if err := AssertAggregateBoundaries(methods, nil); err != nil {
			t.Fatalf("a non-aggregate service must be unaffected, got %v", err)
		}
	})

	// AIP-137 batch write (the round-2 fail-open hole): a member that registers a
	// BatchCreate/BatchUpdate/BatchDelete must fail the gate just like a standard
	// write — the generated svc records batch writes in WriteMethods so the gate's
	// intersection catches them. BatchGet (a read) is addressable and must NOT be in
	// WriteMethods, so it never trips the gate.
	t.Run("member batch write registered → fails", func(t *testing.T) {
		const (
			batchCreate = "/order.v1.ItemService/BatchCreateItems"
			batchGet    = "/order.v1.ItemService/BatchGetItems"
		)
		methods := []string{batchCreate, batchGet, getItem, listItems}
		members := []MemberBinding{{
			Resource:     "Item",
			Root:         "Order",
			WriteMethods: []string{batchCreate}, // batch READ intentionally absent
		}}
		err := AssertAggregateBoundaries(methods, members)
		if err == nil {
			t.Fatal("a registered member BATCH write must fail the boundary gate")
		}
		if !strings.Contains(err.Error(), batchCreate) {
			t.Fatalf("error should name the offending batch write: %v", err)
		}
		if strings.Contains(err.Error(), batchGet) {
			t.Fatalf("BatchGet is a read and must not be reported as a violation: %v", err)
		}
	})
}

// TestServeRunsAggregateGate verifies the gate is wired into the Server's
// accumulator: a recorded member binding whose write method is also a recorded
// method makes AssertAggregateBoundaries (over the server's accumulated state)
// fail, mirroring how Serve invokes it.
func TestServeRunsAggregateGate(t *testing.T) {
	s := &Server{}
	s.RecordMethods("/order.v1.ItemService/CreateItem", "/order.v1.ItemService/GetItem")
	s.RecordMemberBinding(MemberBinding{
		Resource:     "Item",
		Root:         "Order",
		WriteMethods: []string{"/order.v1.ItemService/CreateItem"},
	})
	if err := AssertAggregateBoundaries(s.methods, s.memberBindings); err == nil {
		t.Fatal("accumulated member write must fail the gate")
	}
}
