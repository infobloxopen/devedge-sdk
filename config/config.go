// Package config provides a stdlib-only, dependency-light configuration seam.
// Core packages use ONLY reflect, strconv, time, os, flag, encoding/json, bufio,
// strings — NO third-party imports. Heavy adapters (YAML, TOML, remote) live in
// sub-packages (e.g. config/koanf) and are opt-in.
//
// Usage:
//
//	var opts config.ServerOptions
//	if err := config.Load(&opts,
//	    config.Flags(fs),
//	    config.Env("MY_SVC_"),
//	    config.DotEnv(".env"),
//	); err != nil {
//	    log.Fatal(err)
//	}
//
// Precedence (highest to lowest): sources listed first win, then default tag.
package config

import (
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// Source is the single abstraction over any config origin (env, flags, file,
// map, remote). Get returns the string value for key and whether it was present.
// Implementations must be safe for concurrent use.
type Source interface {
	Get(key string) (value string, ok bool)
}

// Load populates the exported, tagged fields of dst (must be a non-nil pointer
// to a struct) from sources in precedence order: the first source that returns
// ok=true for a key wins. If no source provides a value and a `default:` tag is
// present, that default is used. Fields without a `config:` tag are skipped.
//
// Supported field kinds: string, int, int64, bool, float64, time.Duration.
// Any other kind returns an error naming the key.
func Load(dst any, sources ...Source) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return fmt.Errorf("config.Load: dst must be a non-nil pointer to struct")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("config.Load: dst must be a pointer to struct, got pointer to %s", v.Kind())
	}
	return loadStruct(v, sources)
}

func loadStruct(v reflect.Value, sources []Source) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		fv := v.Field(i)
		ft := t.Field(i)

		// Skip unexported fields.
		if !fv.CanSet() {
			continue
		}

		// Recurse into embedded (anonymous) structs.
		if ft.Anonymous && fv.Kind() == reflect.Struct {
			if err := loadStruct(fv, sources); err != nil {
				return err
			}
			continue
		}

		key, hasKey := ft.Tag.Lookup("config")
		if !hasKey || key == "" {
			continue
		}
		defaultVal := ft.Tag.Get("default")

		raw, found := resolve(key, defaultVal, sources)
		if !found {
			continue
		}

		if err := setField(fv, key, raw); err != nil {
			return err
		}
	}
	return nil
}

// resolve returns the value for key in precedence order: the first source that
// reports the key as present (ok=true) wins — including a source that returns an
// explicitly empty value, which correctly overrides later sources and the
// default. Only when NO source has the key does it fall back to defaultVal.
//
// It always returns found=true so the caller always invokes setField. That is
// safe because defaultVal is ft.Tag.Get("default"), which is "" both for a
// missing tag and for an explicit `default:""`; setField treats an empty raw
// value as "leave the field at its zero value" for non-string kinds (so an
// untagged int is not spuriously zeroed by a malformed parse) and as the empty
// string for string kinds (the documented `default:""` semantics for fields
// like OTLPEndpoint/DSN). The distinction between the two cases is therefore
// immaterial to the result.
func resolve(key, defaultVal string, sources []Source) (string, bool) {
	for _, s := range sources {
		if v, ok := s.Get(key); ok {
			return v, true
		}
	}
	return defaultVal, true
}

var durationType = reflect.TypeOf(time.Duration(0))

func setField(fv reflect.Value, key, raw string) error {
	// Handle time.Duration before the kind switch (it's an int64 alias).
	if fv.Type() == durationType {
		if raw == "" {
			return nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("config: key %q: cannot parse %q as time.Duration: %w", key, raw, err)
		}
		fv.Set(reflect.ValueOf(d))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if raw == "" {
			return nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("config: key %q: cannot parse %q as %s", key, raw, fv.Kind())
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if raw == "" {
			return nil
		}
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("config: key %q: cannot parse %q as %s", key, raw, fv.Kind())
		}
		fv.SetUint(n)
	case reflect.Bool:
		if raw == "" {
			return nil
		}
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("config: key %q: cannot parse %q as bool", key, raw)
		}
		fv.SetBool(b)
	case reflect.Float32, reflect.Float64:
		if raw == "" {
			return nil
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("config: key %q: cannot parse %q as %s", key, raw, fv.Kind())
		}
		fv.SetFloat(f)
	default:
		return fmt.Errorf("config: key %q: unsupported field kind %s", key, fv.Kind())
	}
	return nil
}
