package devsvc

import (
	"context"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// LoadGrantsFile reads a YAML (or JSON — YAML is a superset) list of authz.Grant
// from path. It is the on-disk form a developer edits to manipulate dev
// authorization; YAML is the friendlier hand-edited default.
//
// Example grants.yaml:
//
//	- tenant: tenant-a
//	  subjects: [group:admin]
//	  verbs: ["*"]
//	  resource: "*"
//	- tenant: "*"
//	  subjects: [group:viewer]
//	  verbs: [get, list]
//	  resource: order
//
// The keys are lowercase snake_case (see authz.Grant's struct tags) and are
// identical whether the file is written as YAML or JSON.
func LoadGrantsFile(path string) ([]authz.Grant, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("devsvc: read grants file %q: %w", path, err)
	}
	var grants []authz.Grant
	if err := yaml.Unmarshal(b, &grants); err != nil {
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
