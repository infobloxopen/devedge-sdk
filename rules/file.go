package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// DefaultPollInterval is the modtime poll cadence used by [NewFileSource] when
// the caller passes a non-positive interval.
const DefaultPollInterval = 5 * time.Second

// FileSource is a zero-dependency [Source] backed by a JSON file that maps
// tenant IDs to rulesets, e.g.
//
//	{"tenant-a": {<ruleset>}, "tenant-b": {<ruleset>}, "": {<default ruleset>}}
//
// It polls the file's modification time on an interval and reloads on change —
// a "file-watch" without an fsnotify dependency. This suits rules delivered as
// a Kubernetes ConfigMap mounted into the pod (the kubelet rewrites the file on
// update, bumping its modtime). An fsnotify-backed source, or a real ConfigMap
// API bridge, is a separate adapter built when needed.
//
// FileSource embeds a [StaticSource] for in-memory storage and watcher fan-out;
// the poll loop reconciles the file's contents into it via Replace, so a Cache
// over a FileSource gets the same Get/Watch/Snapshot behaviour as over any
// other source.
//
// The zero value is not usable; construct with [NewFileSource].
type FileSource[T any] struct {
	*StaticSource[T]
	path     string
	interval time.Duration

	mu      sync.Mutex
	lastMod time.Time
	loaded  bool
}

// NewFileSource returns a file-backed source reading path. A non-positive
// interval defaults to [DefaultPollInterval]. Call [FileSource.Run] to start
// polling; call [FileSource.Load] once first if you need the data present
// synchronously before serving.
func NewFileSource[T any](path string, interval time.Duration) *FileSource[T] {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	return &FileSource[T]{
		StaticSource: NewStaticSource[T](),
		path:         path,
		interval:     interval,
	}
}

// Load reads the file and reconciles it into the in-memory store if the file's
// modtime has changed since the last load (or on the first call). It is safe to
// call repeatedly. A read or parse error is returned and leaves the last
// good state intact — fail-safe: a momentarily unreadable or malformed file
// does not wipe loaded rules.
func (f *FileSource[T]) Load() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	info, err := os.Stat(f.path)
	if err != nil {
		return fmt.Errorf("rules: file source %q: stat: %w", f.path, err)
	}
	mod := info.ModTime()
	if f.loaded && mod.Equal(f.lastMod) {
		return nil // unchanged
	}

	raw, err := os.ReadFile(f.path)
	if err != nil {
		return fmt.Errorf("rules: file source %q: read: %w", f.path, err)
	}
	next := make(map[string]T)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &next); err != nil {
			return fmt.Errorf("rules: file source %q: parse: %w", f.path, err)
		}
	}

	f.Replace(next) // broadcasts the diff to watchers
	f.lastMod = mod
	f.loaded = true
	return nil
}

// Snapshot ensures the file has been read at least once, then returns the
// in-memory snapshot. It overrides the embedded [StaticSource.Snapshot] so a
// [Cache]'s initial load reflects the file (and only reports ready once the
// file has been read). If the file cannot be read and nothing has loaded yet,
// the read error propagates and the cache stays not-ready; if a prior good load
// exists, that stale-but-good snapshot is returned (fail-safe).
func (f *FileSource[T]) Snapshot(ctx context.Context) (map[string]T, uint64, error) {
	if err := f.Load(); err != nil {
		f.mu.Lock()
		loaded := f.loaded
		f.mu.Unlock()
		if !loaded {
			return nil, 0, err
		}
		// A prior good load exists: serve last-known-good.
	}
	return f.StaticSource.Snapshot(ctx)
}

// Run loads once, then polls for modtime changes on the configured interval and
// reloads on change, until ctx is cancelled. It blocks; run it in a goroutine.
// Poll errors (a transiently missing or malformed file) are swallowed so a bad
// write does not stop the loop or clear loaded rules; the next good poll
// recovers. Run returns the context error on cancellation.
func (f *FileSource[T]) Run(ctx context.Context) error {
	_ = f.Load() // best-effort initial load; Cache stays not-ready until it succeeds
	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			_ = f.Load()
		}
	}
}
