package rules_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/rules"
)

func TestStaticSource_GetSetDelete(t *testing.T) {
	s := rules.NewStaticSource[int]()
	if _, err := s.Get(context.Background(), "t1"); !errors.Is(err, rules.ErrNotFound) {
		t.Fatalf("Get on empty: err=%v, want ErrNotFound", err)
	}
	s.Set("t1", 7)
	if v, err := s.Get(context.Background(), "t1"); err != nil || v != 7 {
		t.Fatalf("Get after Set: v=%d err=%v, want 7,nil", v, err)
	}
	s.Delete("t1")
	if _, err := s.Get(context.Background(), "t1"); !errors.Is(err, rules.ErrNotFound) {
		t.Fatalf("Get after Delete: err=%v, want ErrNotFound", err)
	}
}

func TestStaticSource_Snapshot(t *testing.T) {
	s := rules.NewStaticSource[string]()
	s.Set("a", "1")
	s.Set("b", "2")
	data, rev, err := s.Snapshot(context.Background())
	if err != nil || len(data) != 2 || data["a"] != "1" || data["b"] != "2" {
		t.Fatalf("snapshot=%v rev=%d err=%v", data, rev, err)
	}
	// Returned map is a copy: mutating it must not affect the source.
	data["a"] = "mutated"
	if v, _ := s.Get(context.Background(), "a"); v != "1" {
		t.Fatalf("snapshot leaked: source a=%q, want 1", v)
	}
}

func TestStaticSource_Replace(t *testing.T) {
	s := rules.NewStaticSource[string]()
	s.Set("keep", "old")
	s.Set("drop", "x")
	s.Replace(map[string]string{"keep": "new", "add": "y"})
	if v, _ := s.Get(context.Background(), "keep"); v != "new" {
		t.Fatalf("keep=%q, want new", v)
	}
	if v, _ := s.Get(context.Background(), "add"); v != "y" {
		t.Fatalf("add=%q, want y", v)
	}
	if _, err := s.Get(context.Background(), "drop"); !errors.Is(err, rules.ErrNotFound) {
		t.Fatalf("drop not removed: err=%v", err)
	}
}

func TestStaticSource_WatchDeliversAndCloses(t *testing.T) {
	s := rules.NewStaticSource[int]()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("t1", 1)
	select {
	case ev := <-ch:
		if ev.Tenant != "t1" || ev.Value != 1 || ev.Deleted {
			t.Fatalf("event=%+v, want t1=1 not-deleted", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no Set event within 1s")
	}
	s.Delete("t1")
	select {
	case ev := <-ch:
		if ev.Tenant != "t1" || !ev.Deleted {
			t.Fatalf("event=%+v, want t1 deleted", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no Delete event within 1s")
	}
	// Cancelling ctx closes the channel.
	cancel()
	select {
	case _, open := <-ch:
		if open {
			// drain a possibly-buffered event, then expect close
			select {
			case _, open2 := <-ch:
				if open2 {
					t.Fatal("watch channel not closed after cancel")
				}
			case <-time.After(time.Second):
				t.Fatal("watch channel not closed after cancel")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("watch channel not closed after cancel")
	}
}
