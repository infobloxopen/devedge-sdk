package persistence

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// DSN resolves to a database driver name and data source name. It supports
// devedge's indirect "hotload" convention so rotated credentials can be reloaded
// without a restart: a DSN may be a literal, read from an environment variable,
// or read from a file whose contents are the real DSN (optionally referenced via
// the indirect form "fsnotify://<driver>/<abs-path>").
//
// Resolve returns the DSN as it currently is; a reload-capable client (e.g.
// infobloxopen/hotload) is the consuming application's concern, not the SDK's.
type DSN interface {
	Resolve(ctx context.Context) (driver string, dsn string, err error)
}

// Literal is a DSN supplied directly.
type Literal struct {
	Driver string
	DSN    string
}

// Resolve implements [DSN].
func (l Literal) Resolve(context.Context) (string, string, error) { return l.Driver, l.DSN, nil }

// FromEnv reads a literal DSN from an environment variable.
type FromEnv struct {
	Driver string
	Var    string
}

// Resolve implements [DSN].
func (e FromEnv) Resolve(context.Context) (string, string, error) {
	v := os.Getenv(e.Var)
	if v == "" {
		return "", "", fmt.Errorf("persistence: env %q is empty", e.Var)
	}
	return e.Driver, v, nil
}

// HotloadFile reads the real DSN from a file (devedge writes one per declared
// dependency). Path may be a plain path or the indirect form
// "fsnotify://<driver>/<abs-path>", matching the devedge connection convention.
type HotloadFile struct {
	Driver string
	Path   string
}

// Resolve implements [DSN].
func (h HotloadFile) Resolve(context.Context) (string, string, error) {
	driver, path := h.Driver, h.Path
	if rest, ok := strings.CutPrefix(path, "fsnotify://"); ok {
		if i := strings.IndexByte(rest, '/'); i > 0 {
			driver, path = rest[:i], rest[i:]
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("persistence: read DSN file: %w", err)
	}
	return driver, strings.TrimSpace(string(b)), nil
}
