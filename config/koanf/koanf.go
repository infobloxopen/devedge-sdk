// Package koanf provides a config.Source adapter over github.com/knadh/koanf/v2.
// This is the ONLY package in devedge-sdk that imports koanf — the core config
// package (github.com/infobloxopen/devedge-sdk/config) is stdlib-only.
//
// Use this adapter when you need YAML, TOML, HCL, remote, or file-watch support:
//
//	import (
//	    "github.com/infobloxopen/devedge-sdk/config"
//	    konfig "github.com/infobloxopen/devedge-sdk/config/koanf"
//	)
//
//	src, err := konfig.YAMLFile("config.yaml")
//	if err != nil { log.Fatal(err) }
//	config.Load(&opts, config.Flags(fs), config.Env("SVC_"), src)
package koanf

import (
	"fmt"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// KoanfSource wraps a loaded *koanf.Koanf and implements config.Source.
// Keys are looked up case-insensitively (koanf lowercases keys by default).
type KoanfSource struct {
	k *koanf.Koanf
}

// Get returns the value for key, converting it to a string. koanf stores
// values as any; this coerces them to string with fmt.Sprintf.
func (s *KoanfSource) Get(key string) (string, bool) {
	lower := strings.ToLower(key)
	v := s.k.Get(lower)
	if v == nil {
		v = s.k.Get(key)
	}
	if v == nil {
		return "", false
	}
	return fmt.Sprintf("%v", v), true
}

// YAMLFile returns a KoanfSource loaded from a YAML file at path.
// Returns an error if the file exists but cannot be parsed. A missing file
// returns an error too — callers that want an optional file should check with
// os.Stat first or use the returned error.
func YAMLFile(path string) (*KoanfSource, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("koanf: load YAML %q: %w", path, err)
	}
	return &KoanfSource{k: k}, nil
}
