package config

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
)

// EnvSource looks up keys in the environment with an optional prefix.
type EnvSource struct{ prefix string }

// Env returns a Source that reads from environment variables. The key is
// looked up as prefix+key (e.g. prefix "APP_" + key "GRPC_ADDR" → "APP_GRPC_ADDR").
func Env(prefix string) Source { return &EnvSource{prefix: prefix} }

func (s *EnvSource) Get(key string) (string, bool) {
	return os.LookupEnv(s.prefix + key)
}

// FlagsSource reads from a *flag.FlagSet, returning values only for flags that
// were explicitly Set on the command line (not just their defaults).
type FlagsSource struct{ fs *flag.FlagSet }

// Flags returns a Source backed by fs. A key matches a flag when the flag name
// equals the key or its lowercase equivalent.
func Flags(fs *flag.FlagSet) Source { return &FlagsSource{fs: fs} }

func (s *FlagsSource) Get(key string) (string, bool) {
	lower := strings.ToLower(key)
	var result string
	var found bool
	// Visit iterates only over flags that were Set.
	s.fs.Visit(func(f *flag.Flag) {
		if f.Name == key || f.Name == lower {
			result = f.Value.String()
			found = true
		}
	})
	return result, found
}

// DotEnvSource lazily reads a .env file on first use.
type DotEnvSource struct {
	path string
	once sync.Once
	m    map[string]string
}

// DotEnv returns a Source that reads from a .env file at path. Blank lines and
// lines starting with # are ignored. If the file does not exist, the source is
// silently empty.
func DotEnv(path string) Source { return &DotEnvSource{path: path} }

func (s *DotEnvSource) load() {
	s.m = make(map[string]string)
	f, err := os.Open(s.path)
	if err != nil {
		return // missing file is not an error
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		// Strip surrounding quotes (single or double).
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		s.m[k] = v
	}
	// A scan error (read failure, or a line longer than bufio.Scanner's 64KB
	// token limit) otherwise silently truncates config — every key past the
	// failing line is dropped with no signal, so a partially-read file would
	// masquerade as a complete one. Discard the partial map: the source then
	// reports every key as absent (ok=false), so defaults / later sources apply
	// uniformly rather than a silent subset of the file winning. (The Source
	// interface has no error channel; a truncated read is treated like a missing
	// file — best-effort, never a misleading partial.)
	if sc.Err() != nil {
		s.m = make(map[string]string)
	}
}

func (s *DotEnvSource) Get(key string) (string, bool) {
	s.once.Do(s.load)
	v, ok := s.m[key]
	return v, ok
}

// JSONFileSource reads a flat JSON object from a file.
type JSONFileSource struct {
	path string
	once sync.Once
	m    map[string]string
}

// JSONFile returns a Source backed by a JSON file at path. The file must contain
// a JSON object; values are coerced to strings. If the file does not exist, the
// source is silently empty.
func JSONFile(path string) Source { return &JSONFileSource{path: path} }

func (s *JSONFileSource) load() {
	s.m = make(map[string]string)
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			s.m[k] = val
		default:
			s.m[k] = fmt.Sprintf("%v", v)
		}
	}
}

func (s *JSONFileSource) Get(key string) (string, bool) {
	s.once.Do(s.load)
	v, ok := s.m[key]
	return v, ok
}

// MapSource is a simple in-memory map source, useful for testing or overrides.
type MapSource struct{ m map[string]string }

// Map returns a Source backed by m.
func Map(m map[string]string) Source { return &MapSource{m: m} }

func (s *MapSource) Get(key string) (string, bool) {
	v, ok := s.m[key]
	return v, ok
}
