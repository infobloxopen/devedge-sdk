package cells

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// fileLockTimeout bounds how long CompareAndSet/Get/Delete wait for the
// cross-process lock before giving up.
const fileLockTimeout = 5 * time.Second

// defaultFilePollInterval is how often a FileTable watch re-reads the file to
// detect changes made by other processes (e.g. the `devedge cell` CLI writing a
// route that a running service's Router must observe).
const defaultFilePollInterval = 500 * time.Millisecond

// fileTableDoc is the on-disk JSON shape.
type fileTableDoc struct {
	Revision uint64                 `json:"revision"`
	Routes   map[string]TenantRoute `json:"routes"`
}

// FileTable is a JSON-file-backed [RoutingTable]: a persistent, dependency-light
// (stdlib-only) backend suitable for local development, where the `devedge cell`
// CLI and a running service share one routes file. Writes are guarded by a
// cross-process advisory lock and applied atomically (write-temp + rename), so a
// CompareAndSet is safe even when several processes share the file. Watch is
// poll-based (it re-reads the file on an interval) since the writer may be a
// different process.
//
// Correctness never depends on the watch being instantaneous: the Router lazily
// re-Gets on a cache miss and every cell re-checks the epoch at L2/L3. A
// Raft/etcd-backed table implements the same interface for production.
type FileTable struct {
	path         string
	pollInterval time.Duration

	mu       sync.Mutex // serializes in-process access; the lock file serializes across processes
	watchers map[int]chan RouteEvent
	nextW    int
}

// FileTableOption configures a [FileTable].
type FileTableOption func(*FileTable)

// WithPollInterval sets how often watches re-read the file (default 500ms).
func WithPollInterval(d time.Duration) FileTableOption {
	return func(t *FileTable) {
		if d > 0 {
			t.pollInterval = d
		}
	}
}

// NewFileTable returns a routing table persisted at path. The file is created on
// the first write; a missing file reads as an empty table.
func NewFileTable(path string, opts ...FileTableOption) *FileTable {
	t := &FileTable{
		path:         path,
		pollInterval: defaultFilePollInterval,
		watchers:     make(map[int]chan RouteEvent),
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// lockPath is the sidecar advisory-lock file.
func (t *FileTable) lockPath() string { return t.path + ".lock" }

// withFileLock runs fn while holding the cross-process advisory lock. The lock is
// an O_CREATE|O_EXCL sidecar file, spun on with a small backoff up to
// fileLockTimeout, then removed.
func (t *FileTable) withFileLock(fn func() error) error {
	deadline := time.Now().Add(fileLockTimeout)
	for {
		f, err := os.OpenFile(t.lockPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			defer os.Remove(t.lockPath())
			return fn()
		}
		if !os.IsExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("cells: timed out acquiring file table lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// loadLocked reads the doc. A missing file is an empty table. Caller holds the
// file lock.
func (t *FileTable) loadLocked() (fileTableDoc, error) {
	doc := fileTableDoc{Routes: make(map[string]TenantRoute)}
	data, err := os.ReadFile(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, err
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, err
	}
	if doc.Routes == nil {
		doc.Routes = make(map[string]TenantRoute)
	}
	return doc, nil
}

// storeLocked writes the doc atomically (temp + rename). Caller holds the lock.
func (t *FileTable) storeLocked(doc fileTableDoc) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

// Get implements [RoutingTable].
func (t *FileTable) Get(_ context.Context, tenantID string) (TenantRoute, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out TenantRoute
	var found bool
	err := t.withFileLock(func() error {
		doc, err := t.loadLocked()
		if err != nil {
			return err
		}
		out, found = doc.Routes[tenantID]
		return nil
	})
	if err != nil {
		return TenantRoute{}, err
	}
	if !found {
		return TenantRoute{}, ErrNoRoute
	}
	return out, nil
}

// CompareAndSet implements [RoutingTable] with the same semantics as [MemTable]:
// the precondition is checked against the stored route's (RouteEpoch, State);
// creation requires expect to be the zero route and the tenant to be absent; a
// lower next.RouteEpoch is rejected with ErrEpochRegression.
func (t *FileTable) CompareAndSet(_ context.Context, expect, next TenantRoute) error {
	if next.TenantID == "" {
		return ErrCASConflict
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.withFileLock(func() error {
		doc, err := t.loadLocked()
		if err != nil {
			return err
		}
		cur, ok := doc.Routes[next.TenantID]
		if !ok {
			if !expect.IsZero() {
				return ErrCASConflict
			}
		} else {
			if cur.RouteEpoch != expect.RouteEpoch || cur.State != expect.State {
				return ErrCASConflict
			}
			if next.RouteEpoch < cur.RouteEpoch {
				return ErrEpochRegression
			}
		}
		doc.Routes[next.TenantID] = next
		doc.Revision++
		return t.storeLocked(doc)
	})
}

// Delete removes a tenant's route (turning off a cell reverts its tenants to the
// default cell). Idempotent. Not part of [RoutingTable] — an admin affordance.
func (t *FileTable) Delete(_ context.Context, tenantID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.withFileLock(func() error {
		doc, err := t.loadLocked()
		if err != nil {
			return err
		}
		if _, ok := doc.Routes[tenantID]; !ok {
			return nil
		}
		delete(doc.Routes, tenantID)
		doc.Revision++
		return t.storeLocked(doc)
	})
}

// List returns a snapshot of every route (admin/status affordance; not part of
// [RoutingTable]). Used by `devedge cell status`.
func (t *FileTable) List(_ context.Context) ([]TenantRoute, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []TenantRoute
	err := t.withFileLock(func() error {
		doc, err := t.loadLocked()
		if err != nil {
			return err
		}
		for _, r := range doc.Routes {
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// Watch implements [RoutingTable] by polling the file. It emits a RouteEvent for
// every tenant whose route changed since the previous poll, and a Deleted event
// for a tenant whose route was removed. Like [MemTable], it does not replay the
// state that already exists when the watch starts (the Router lazily Gets on a
// miss). The channel is closed when ctx is cancelled.
func (t *FileTable) Watch(ctx context.Context) (<-chan RouteEvent, error) {
	t.mu.Lock()
	id := t.nextW
	t.nextW++
	ch := make(chan RouteEvent, memWatchBuffer)
	t.watchers[id] = ch
	t.mu.Unlock()

	// Seed the snapshot with the current state so we only emit future changes.
	prev := map[string]TenantRoute{}
	var prevRev uint64
	_ = t.withFileLock(func() error {
		doc, err := t.loadLocked()
		if err != nil {
			return err
		}
		prev = doc.Routes
		prevRev = doc.Revision
		return nil
	})

	go func() {
		defer func() {
			t.mu.Lock()
			delete(t.watchers, id)
			close(ch)
			t.mu.Unlock()
		}()
		ticker := time.NewTicker(t.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			var doc fileTableDoc
			if err := t.withFileLock(func() error {
				d, err := t.loadLocked()
				doc = d
				return err
			}); err != nil {
				continue
			}
			if doc.Revision == prevRev {
				continue
			}
			// Emit changes and deletions, then advance the snapshot.
			for tid, r := range doc.Routes {
				if old, ok := prev[tid]; !ok || old != r {
					select {
					case ch <- RouteEvent{Route: r, TenantID: tid, Revision: doc.Revision}:
					case <-ctx.Done():
						return
					}
				}
			}
			for tid := range prev {
				if _, ok := doc.Routes[tid]; !ok {
					select {
					case ch <- RouteEvent{TenantID: tid, Deleted: true, Revision: doc.Revision}:
					case <-ctx.Done():
						return
					}
				}
			}
			prev = doc.Routes
			prevRev = doc.Revision
		}
	}()
	return ch, nil
}
