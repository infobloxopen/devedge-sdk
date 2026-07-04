package devsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// LoadGrantsFile reads a JSON array of authz.Grant from path. It is the on-disk
// form a developer edits to manipulate dev authorization.
//
// Example grants.json:
//
//	[
//	  {"Tenant":"tenant-a","Subjects":["group:admin"],"Verbs":["*"],"Resource":"*"},
//	  {"Tenant":"*","Subjects":["group:viewer"],"Verbs":["get","list"],"Resource":"order"}
//	]
func LoadGrantsFile(path string) ([]authz.Grant, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("devsvc: read grants file %q: %w", path, err)
	}
	var grants []authz.Grant
	if err := json.Unmarshal(b, &grants); err != nil {
		return nil, fmt.Errorf("devsvc: parse grants file %q: %w", path, err)
	}
	return grants, nil
}

// WatchGrantsFile reloads store from path whenever the file's modification time
// changes, polling every interval (zero-dependency hot-reload — no fsnotify). It
// loads once immediately, then runs until ctx is cancelled. A load error after
// the initial load is passed to onErr (if non-nil) and the last-good grants are
// kept — a bad edit never takes the service down. Returns the initial load error
// so a caller can fail fast on a broken file at startup.
func WatchGrantsFile(ctx context.Context, path string, store *Store, interval time.Duration, onErr func(error)) error {
	if interval <= 0 {
		interval = time.Second
	}
	grants, err := LoadGrantsFile(path)
	if err != nil {
		return err
	}
	store.Replace(grants...)
	last := modTime(path)

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mt := modTime(path)
				if mt.Equal(last) {
					continue
				}
				last = mt
				g, lerr := LoadGrantsFile(path)
				if lerr != nil {
					if onErr != nil {
						onErr(lerr)
					}
					continue // keep last-good grants
				}
				store.Replace(g...)
			}
		}
	}()
	return nil
}

func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
