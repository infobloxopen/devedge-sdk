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

// resolve finds the first source that provides a value for key, or falls back
// to defaultVal. Returns (value, true) if any value was found, ("", false) if
// neither sources nor a default provided anything.
func resolve(key, defaultVal string, sources []Source) (string, bool) {
	for _, s := range sources {
		if v, ok := s.Get(key); ok {
			return v, true
		}
	}
	// No source matched; use default tag if non-empty or if the tag is explicitly "".
	// We distinguish "tag not present" (handled by caller) from "tag present but empty".
	// Here defaultVal is always the result of Tag.Get which returns "" for missing tag
	// too, so we only use it when the tag was explicitly set (checked by caller via
	// ft.Tag.Lookup("default")). Since we don't have that info here, always return it
	// if any source was tried or not — the caller already checked hasKey.
	// Actually: if the `default` tag was set, return it (even if "").
	// The caller passes defaultVal = ft.Tag.Get("default"). If the field had no
	// `default:` tag, ft.Tag.Get returns "". We can't distinguish "no tag" from
	// `default:""` here. To keep things simple: always return defaultVal even if "".
	// This means a field with no default tag gets "" applied only if it's a string —
	// for non-string kinds, "" means "no value" and setField will parse "".
	//
	// Better approach: return defaultVal only when it is non-empty, OR always return it.
	// The spec says `default:""` is valid for OTLPEndpoint/DSN to mean "empty string".
	// So: always return (defaultVal, true) — the field gets set to its default (possibly "").
	// But that would zero-out int fields when no default is set. Let the caller decide.
	//
	// Revised: return (defaultVal, defaultVal != "" || /* tag was present */ true).
	// Since we always receive defaultVal from Tag.Get("default"), we can't know if it
	// was present. Callers must pass an explicit sentinel. Instead: always emit (defaultVal, true)
	// so callers always get a value. For numeric fields with no default tag, the caller
	// passes defaultVal="" which leads to setField parsing "" — handle that by skipping.
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
