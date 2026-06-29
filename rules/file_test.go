package rules_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/rules"
)

func writeFile(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestFileSource_LoadAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	base := time.Now().Add(-time.Hour)
	writeFile(t, path, `{"t1": 1, "t2": 2}`, base)

	fs := rules.NewFileSource[int](path, 0)
	if err := fs.Load(); err != nil {
		t.Fatalf("initial Load: %v", err)
	}
	if v, err := fs.Get(context.Background(), "t1"); err != nil || v != 1 {
		t.Fatalf("Get(t1)=%d,%v want 1,nil", v, err)
	}
	data, _, _ := fs.Snapshot(context.Background())
	if len(data) != 2 {
		t.Fatalf("snapshot len=%d, want 2", len(data))
	}

	// Same mtime → Load is a no-op (returns nil, keeps state).
	if err := fs.Load(); err != nil {
		t.Fatalf("no-op Load: %v", err)
	}

	// Rewrite with a later mtime → reload picks up changes (t2 dropped, t3 added).
	writeFile(t, path, `{"t1": 11, "t3": 3}`, base.Add(time.Minute))
	if err := fs.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if v, _ := fs.Get(context.Background(), "t1"); v != 11 {
		t.Fatalf("after reload t1=%d, want 11", v)
	}
	if _, err := fs.Get(context.Background(), "t2"); err == nil {
		t.Fatal("t2 should be removed after reload")
	}
	if v, _ := fs.Get(context.Background(), "t3"); v != 3 {
		t.Fatalf("t3=%d, want 3", v)
	}
}

func TestFileSource_FailSafeOnBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	base := time.Now().Add(-time.Hour)
	writeFile(t, path, `{"t1": 1}`, base)

	fs := rules.NewFileSource[int](path, 0)
	if err := fs.Load(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the file with a newer mtime: Load errors, prior data is kept.
	writeFile(t, path, `{ not json`, base.Add(time.Minute))
	if err := fs.Load(); err == nil {
		t.Fatal("Load should error on malformed JSON")
	}
	if v, err := fs.Get(context.Background(), "t1"); err != nil || v != 1 {
		t.Fatalf("after bad reload t1=%d,%v want 1,nil (last-known-good)", v, err)
	}

	// Snapshot serves last-known-good even though the current file is bad.
	if data, _, err := fs.Snapshot(context.Background()); err != nil || data["t1"] != 1 {
		t.Fatalf("snapshot fail-safe: data=%v err=%v", data, err)
	}
}

func TestFileSource_CacheReadyOnlyAfterLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")

	// File does not exist yet.
	fs := rules.NewFileSource[int](path, 10*time.Millisecond)
	c := rules.NewCache("file", fs)
	ctx := t.Context()
	go func() { _ = fs.Run(ctx) }()
	go func() { _ = c.Run(ctx) }()

	time.Sleep(40 * time.Millisecond)
	if c.Ready() {
		t.Fatal("cache ready before the file exists")
	}

	// Create the file → poller loads it → cache becomes ready with data.
	writeFile(t, path, `{"t1": 5}`, time.Now())
	waitFor(t, "ready after file appears", c.Ready)
	if v, ok := c.Get("t1"); !ok || v != 5 {
		t.Fatalf("Get(t1)=%d,%v want 5,true", v, ok)
	}
}
