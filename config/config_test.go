package config_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/config"
)

// TestLoad_Precedence verifies that earlier sources win over later ones.
func TestLoad_Precedence(t *testing.T) {
	type opts struct {
		Addr string `config:"ADDR" default:"default"`
	}
	mapA := config.Map(map[string]string{"ADDR": "from-map-a"})
	mapB := config.Map(map[string]string{"ADDR": "from-map-b"})

	var o opts
	if err := config.Load(&o, mapA, mapB); err != nil {
		t.Fatal(err)
	}
	if o.Addr != "from-map-a" {
		t.Errorf("expected %q, got %q", "from-map-a", o.Addr)
	}
}

// TestLoad_Default verifies the default tag is used when no source provides a value.
func TestLoad_Default(t *testing.T) {
	type opts struct {
		Addr string `config:"ADDR" default:":9090"`
	}
	var o opts
	if err := config.Load(&o); err != nil {
		t.Fatal(err)
	}
	if o.Addr != ":9090" {
		t.Errorf("expected %q, got %q", ":9090", o.Addr)
	}
}

// TestLoad_TypedParsing verifies each supported typed field kind.
func TestLoad_TypedParsing(t *testing.T) {
	type opts struct {
		S   string        `config:"S" default:"hello"`
		I   int           `config:"I" default:"42"`
		I64 int64         `config:"I64" default:"9876543210"`
		B   bool          `config:"B" default:"true"`
		F   float64       `config:"F" default:"3.14"`
		D   time.Duration `config:"D" default:"5s"`
	}
	var o opts
	if err := config.Load(&o); err != nil {
		t.Fatal(err)
	}
	if o.S != "hello" {
		t.Errorf("S: expected %q got %q", "hello", o.S)
	}
	if o.I != 42 {
		t.Errorf("I: expected 42 got %d", o.I)
	}
	if o.I64 != 9876543210 {
		t.Errorf("I64: expected 9876543210 got %d", o.I64)
	}
	if !o.B {
		t.Errorf("B: expected true got false")
	}
	if o.F != 3.14 {
		t.Errorf("F: expected 3.14 got %f", o.F)
	}
	if o.D != 5*time.Second {
		t.Errorf("D: expected 5s got %v", o.D)
	}
}

// TestLoad_ErrorOnMalformed verifies a clear error naming the key on bad values.
func TestLoad_ErrorOnMalformed(t *testing.T) {
	type opts struct {
		Port int `config:"PORT"`
	}
	var o opts
	err := config.Load(&o, config.Map(map[string]string{"PORT": "not-a-number"}))
	if err == nil {
		t.Fatal("expected error for malformed int, got nil")
	}
	// Error must mention the key name.
	if !containsStr(err.Error(), "PORT") {
		t.Errorf("error %q does not mention key name PORT", err.Error())
	}
}

// TestLoad_ErrorOnUnsupportedKind verifies error (not panic) on unsupported kinds.
func TestLoad_ErrorOnUnsupportedKind(t *testing.T) {
	type opts struct {
		Ch chan int `config:"CHAN"`
	}
	var o opts
	err := config.Load(&o, config.Map(map[string]string{"CHAN": "x"}))
	if err == nil {
		t.Fatal("expected error for unsupported kind chan, got nil")
	}
}

// TestLoad_SkipsNoConfigTag verifies that fields without config: tag are untouched.
func TestLoad_SkipsNoConfigTag(t *testing.T) {
	type opts struct {
		Tagged   string `config:"TAGGED"`
		Untagged string // no config tag
	}
	var o opts
	o.Untagged = "original"
	if err := config.Load(&o, config.Map(map[string]string{"TAGGED": "set", "UNTAGGED": "should-not-appear"})); err != nil {
		t.Fatal(err)
	}
	if o.Tagged != "set" {
		t.Errorf("Tagged: expected %q got %q", "set", o.Tagged)
	}
	if o.Untagged != "original" {
		t.Errorf("Untagged: expected %q got %q (should be untouched)", "original", o.Untagged)
	}
}

// TestEnvSource verifies Env(prefix) reads from environment variables.
func TestEnvSource(t *testing.T) {
	const key = "DEVEDGE_TEST_ENV_ADDR"
	t.Setenv(key, ":7070")

	type opts struct {
		Addr string `config:"ADDR"`
	}
	var o opts
	if err := config.Load(&o, config.Env("DEVEDGE_TEST_ENV_")); err != nil {
		t.Fatal(err)
	}
	if o.Addr != ":7070" {
		t.Errorf("expected :7070, got %q", o.Addr)
	}
}

// TestFlagsSource verifies Flags reads from set (not default) flags.
func TestFlagsSource(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	addr := fs.String("grpc_addr", ":9090", "grpc address")
	if err := fs.Parse([]string{"-grpc_addr", ":8181"}); err != nil {
		t.Fatal(err)
	}
	_ = addr

	type opts struct {
		Addr string `config:"grpc_addr" default:":9090"`
	}
	var o opts
	if err := config.Load(&o, config.Flags(fs)); err != nil {
		t.Fatal(err)
	}
	if o.Addr != ":8181" {
		t.Errorf("expected :8181, got %q", o.Addr)
	}
}

// TestFlagsSource_UnsetFlagDoesNotOverride verifies unset flags don't override later sources.
func TestFlagsSource_UnsetFlagDoesNotOverride(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.String("grpc_addr", ":9090", "grpc address")
	// Don't parse any args — flag is at its default, not "set".
	_ = fs.Parse([]string{})

	type opts struct {
		Addr string `config:"grpc_addr" default:":9090"`
	}
	var o opts
	// Flags source first (not set), then env source with a value.
	if err := config.Load(&o,
		config.Flags(fs),
		config.Map(map[string]string{"grpc_addr": ":7777"}),
	); err != nil {
		t.Fatal(err)
	}
	// env (map) should win since the flag was not explicitly set.
	if o.Addr != ":7777" {
		t.Errorf("expected :7777 from map, got %q", o.Addr)
	}
}

// TestDotEnvSource verifies DotEnv reads key=value pairs from a file.
func TestDotEnvSource(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("GRPC_ADDR=:5555\n# comment\n\nHTTP_ADDR=:6666\n"), 0600); err != nil {
		t.Fatal(err)
	}

	type opts struct {
		GRPCAddr string `config:"GRPC_ADDR" default:":9090"`
		HTTPAddr string `config:"HTTP_ADDR" default:":8080"`
	}
	var o opts
	if err := config.Load(&o, config.DotEnv(envFile)); err != nil {
		t.Fatal(err)
	}
	if o.GRPCAddr != ":5555" {
		t.Errorf("GRPCAddr: expected :5555 got %q", o.GRPCAddr)
	}
	if o.HTTPAddr != ":6666" {
		t.Errorf("HTTPAddr: expected :6666 got %q", o.HTTPAddr)
	}
}

// TestDotEnvSource_MissingFile verifies a missing .env file is silently ignored.
func TestDotEnvSource_MissingFile(t *testing.T) {
	type opts struct {
		Addr string `config:"ADDR" default:"default-addr"`
	}
	var o opts
	if err := config.Load(&o, config.DotEnv("/nonexistent/.env")); err != nil {
		t.Fatal(err)
	}
	if o.Addr != "default-addr" {
		t.Errorf("expected default-addr, got %q", o.Addr)
	}
}

// TestDotEnvSource_OverlongLineDoesNotTruncate verifies that a line exceeding
// bufio.Scanner's 64KB token limit does not silently truncate the rest of the
// file: rather than returning a partial subset (which would masquerade as a
// complete read), the source discards the partial map so defaults still apply.
func TestDotEnvSource_OverlongLineDoesNotTruncate(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	// First a valid key, then a line far longer than the 64KB scanner token
	// limit, then a key that would be dropped on a truncated scan.
	huge := strings.Repeat("x", 70*1024)
	content := "GRPC_ADDR=:5555\nBIG=" + huge + "\nHTTP_ADDR=:6666\n"
	if err := os.WriteFile(envFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	type opts struct {
		GRPCAddr string `config:"GRPC_ADDR" default:":9090"`
		HTTPAddr string `config:"HTTP_ADDR" default:":8080"`
	}
	var o opts
	if err := config.Load(&o, config.DotEnv(envFile)); err != nil {
		t.Fatal(err)
	}
	// The overlong line trips sc.Err(); the source must NOT return a truncated
	// subset (e.g. only GRPC_ADDR). It discards everything, so both fields fall
	// back to their defaults — a safe, non-misleading outcome.
	if o.GRPCAddr != ":9090" {
		t.Errorf("GRPCAddr: expected default :9090 after scan error, got %q (silent truncation?)", o.GRPCAddr)
	}
	if o.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr: expected default :8080 after scan error, got %q", o.HTTPAddr)
	}
}

// TestDotEnvSource_SetEmptyOverridesDefault verifies the empty-vs-unset
// distinction: a key present with an empty value wins (ok=true) over a later
// source / the default tag, while an absent key falls through.
func TestDotEnvSource_SetEmptyOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("DSN=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	type opts struct {
		DSN   string `config:"DSN" default:"fallback-dsn"`
		Other string `config:"OTHER" default:"other-default"`
	}
	var o opts
	if err := config.Load(&o, config.DotEnv(envFile)); err != nil {
		t.Fatal(err)
	}
	// DSN is present-but-empty → the empty value wins over the default.
	if o.DSN != "" {
		t.Errorf("DSN: expected empty (present-empty wins), got %q", o.DSN)
	}
	// OTHER is absent → falls through to the default.
	if o.Other != "other-default" {
		t.Errorf("Other: expected default (absent key), got %q", o.Other)
	}
}

// TestEnvSource_UnsetVsSetEmpty verifies Env uses os.LookupEnv semantics: a set
// but empty env var wins over the default; an unset one falls through.
func TestEnvSource_UnsetVsSetEmpty(t *testing.T) {
	t.Setenv("DEVEDGE_TEST_EMPTY_DSN", "") // set to empty
	type opts struct {
		DSN  string `config:"DSN" default:"fallback"`
		Addr string `config:"ADDR" default:":9090"` // ADDR is never set
	}
	var o opts
	if err := config.Load(&o, config.Env("DEVEDGE_TEST_EMPTY_")); err != nil {
		t.Fatal(err)
	}
	if o.DSN != "" {
		t.Errorf("DSN: set-empty env must win over default, got %q", o.DSN)
	}
	if o.Addr != ":9090" {
		t.Errorf("Addr: unset env must fall through to default, got %q", o.Addr)
	}
}

// TestJSONFileSource verifies JSONFile reads key/value pairs from a JSON object.
func TestJSONFileSource(t *testing.T) {
	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(jsonFile, []byte(`{"GRPC_ADDR":":4444","HTTP_ADDR":":5555"}`), 0600); err != nil {
		t.Fatal(err)
	}

	type opts struct {
		GRPCAddr string `config:"GRPC_ADDR" default:":9090"`
		HTTPAddr string `config:"HTTP_ADDR" default:":8080"`
	}
	var o opts
	if err := config.Load(&o, config.JSONFile(jsonFile)); err != nil {
		t.Fatal(err)
	}
	if o.GRPCAddr != ":4444" {
		t.Errorf("GRPCAddr: expected :4444 got %q", o.GRPCAddr)
	}
	if o.HTTPAddr != ":5555" {
		t.Errorf("HTTPAddr: expected :5555 got %q", o.HTTPAddr)
	}
}

// TestMapSource verifies the Map source.
func TestMapSource(t *testing.T) {
	type opts struct {
		X string `config:"X"`
	}
	var o opts
	if err := config.Load(&o, config.Map(map[string]string{"X": "hello"})); err != nil {
		t.Fatal(err)
	}
	if o.X != "hello" {
		t.Errorf("expected hello got %q", o.X)
	}
}

// TestLoad_FullPrecedenceChain tests flags > env > dotenv > default all in one call.
func TestLoad_FullPrecedenceChain(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("GRPC_ADDR=:1111\nHTTP_ADDR=:2222\nLOG_LEVEL=warn\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Set env var for HTTP_ADDR — should win over dotenv.
	t.Setenv("TSVC_HTTP_ADDR", ":3333")
	// Set flag for GRPC_ADDR — should win over env.
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.String("GRPC_ADDR", ":9090", "")
	if err := fs.Parse([]string{"-GRPC_ADDR", ":4444"}); err != nil {
		t.Fatal(err)
	}

	var opts config.ServerOptions
	if err := config.Load(&opts,
		config.Flags(fs),
		config.Env("TSVC_"),
		config.DotEnv(envFile),
	); err != nil {
		t.Fatal(err)
	}
	// Flag wins for GRPC_ADDR.
	if opts.GRPCAddr != ":4444" {
		t.Errorf("GRPCAddr: expected :4444 (flag), got %q", opts.GRPCAddr)
	}
	// Env wins for HTTP_ADDR (env beats dotenv).
	if opts.HTTPAddr != ":3333" {
		t.Errorf("HTTPAddr: expected :3333 (env), got %q", opts.HTTPAddr)
	}
	// DotEnv provides LOG_LEVEL (no flag, no env).
	if opts.LogLevel != "warn" {
		t.Errorf("LogLevel: expected warn (dotenv), got %q", opts.LogLevel)
	}
}

// TestLoad_NotAPointer verifies Load returns an error for non-pointer dst.
func TestLoad_NotAPointer(t *testing.T) {
	type opts struct {
		X string `config:"X"`
	}
	err := config.Load(opts{})
	if err == nil {
		t.Fatal("expected error for non-pointer dst")
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
