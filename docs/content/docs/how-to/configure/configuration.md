---
title: "Configuration"
weight: 1
aliases:
  - /docs/guides/configuration/
---

# Configuration

The `config` package loads service settings from flags, environment variables, `.env` files, and
JSON files. It merges these sources in a defined priority order and maps the results onto a typed
Go struct. Use it whenever you need to configure a service's addresses, log level, database
connection string, or any other setting that varies between environments.

The core package uses the Go standard library only. No third-party config library enters a
consumer's dependency graph unless they explicitly opt into the `config/koanf` adapter described
below.

## Source precedence

```
flags  >  environment  >  file (.env / JSON)  >  default tag
```

Sources are evaluated left to right. The first source that **has** a key wins — the rule is
*presence*, not non-emptiness. Once a source provides a key, later sources are not consulted for
that key.

{{< callout type="info" >}}
**An explicitly empty value still wins.** If `MYSVC_DSN=` appears in the environment (read via
`os.LookupEnv`), that empty string takes precedence over file sources and the `default:` tag.
Only an absent key falls through to the next source.
{{< /callout >}}

## Quick start

```go
import "github.com/infobloxopen/devedge-sdk/config"

var opts config.ServerOptions  // GRPCAddr, HTTPAddr, LogLevel, OTLPEndpoint, DSN
if err := config.Load(&opts,
    config.Flags(fs),          // highest: explicitly-set CLI flags
    config.Env("MYSVC_"),      // env var MYSVC_GRPC_ADDR, MYSVC_HTTP_ADDR, ...
    config.DotEnv(".env"),     // .env file (KEY=VALUE lines)
); err != nil {
    log.Fatal(err)
}
// opts.GRPCAddr, opts.HTTPAddr etc. are now populated
```

The scaffolded `server/main.go` uses this pattern. The addresses, log level, OTel (OpenTelemetry)
endpoint, and database connection string all come from `config.Load`, not hardcoded strings.

## Built-in sources

| Constructor | Reads from |
|---|---|
| `config.Env(prefix)` | `os.LookupEnv(prefix + key)` |
| `config.Flags(fs)` | `*flag.FlagSet` — only flags explicitly _set_ on the command line (not defaults) |
| `config.DotEnv(path)` | `.env` file at path (`KEY=VALUE` or `KEY="VALUE"` lines; missing file silently empty) |
| `config.JSONFile(path)` | Flat JSON object at path (missing file silently empty) |
| `config.Map(m)` | In-memory `map[string]string` (useful for tests) |

The core `config` package imports only Go standard library packages (`reflect`, `strconv`, `time`,
`os`, `flag`, `encoding/json`, `bufio`). Opting into YAML or TOML support requires the separate
`config/koanf` adapter described below.

## Defining your own options struct

Tag any struct with `config:"KEY"` and `default:"..."`:

```go
type MyOptions struct {
    GRPCAddr  string        `config:"GRPC_ADDR"  default:":9090"`
    Timeout   time.Duration `config:"TIMEOUT"    default:"30s"`
    Debug     bool          `config:"DEBUG"      default:"false"`
    Workers   int           `config:"WORKERS"    default:"4"`
}
```

Supported field types: `string`, `int`, `int64`, `bool`, `float64`, `time.Duration`.

- An unsupported field kind returns an error; it does not panic.
- A malformed value returns a descriptive error naming the key (for example,
  `config: key "WORKERS": cannot parse "bad" as int`).
- Fields without a `config:` tag are silently skipped.
- Embedded structs are flattened recursively.

## Canonical `ServerOptions`

The SDK provides a ready-made struct for the settings every service needs:

```go
type ServerOptions struct {
    GRPCAddr     string `config:"GRPC_ADDR"     default:":9090"`
    HTTPAddr     string `config:"HTTP_ADDR"     default:":8080"`
    LogLevel     string `config:"LOG_LEVEL"     default:"info"`
    OTLPEndpoint string `config:"OTLP_ENDPOINT" default:""`
    DSN          string `config:"DSN"           default:""`
}
```

`OTLPEndpoint` feeds `otel.Setup(...OTLPEndpoint...)` in the scaffold. When empty, the standard
`OTEL_EXPORTER_OTLP_ENDPOINT` environment variable is honoured (OTel convention). `DSN` is the
database connection string; an empty value causes the scaffold to fall back to its built-in
default (in-memory SQLite).

## Combining custom settings with `ServerOptions`

A service usually needs its own settings — an app JWKS URL, issuer, audience, or a dev-authz base
URL — alongside the canonical ones. Load them through the same sources so they share one precedence
chain. There are two ways to do it.

### Embed `ServerOptions` in one struct

`config.Load` flattens embedded structs, so embedding `config.ServerOptions` in your own struct
populates both from a single `Load` call:

```go
type Options struct {
    config.ServerOptions // GRPC_ADDR, HTTP_ADDR, LOG_LEVEL, OTLP_ENDPOINT, DSN

    AppJWKSURL  string `config:"APP_JWKS_URL"  default:""`
    AppIssuer   string `config:"APP_ISSUER"    default:""`
    AppAudience string `config:"APP_AUDIENCE"  default:""`
    DevAuthzURL string `config:"DEVAUTHZ_URL"  default:""`
}

var opts Options
if err := config.Load(&opts,
    config.Flags(fs),
    config.Env("MYSVC_"),
    config.DotEnv(".env"),
); err != nil {
    log.Fatal(err)
}
// opts.GRPCAddr and opts.AppJWKSURL are both populated, under the same
// flags > env > file > default precedence.
```

The embedded keys keep their names, so the environment variables are `MYSVC_GRPC_ADDR`,
`MYSVC_APP_JWKS_URL`, and so on.

### Make two `Load` calls against the same sources

When the custom settings live in their own struct or package, build the source list once and load
each struct from it. Sources are read-only, so the same list drives both calls with identical
precedence:

```go
sources := []config.Source{
    config.Flags(fs),
    config.Env("MYSVC_"),
    config.DotEnv(".env"),
}

var srv config.ServerOptions
if err := config.Load(&srv, sources...); err != nil {
    log.Fatal(err)
}

var auth AuthOptions // your own config:"..."-tagged struct
if err := config.Load(&auth, sources...); err != nil {
    log.Fatal(err)
}
```

Both approaches give the custom settings the same flag, environment, `.env`, and default precedence
as `ServerOptions`. Prefer embedding for one config object; use two calls to keep a package's
settings in its own struct.

{{< callout type="info" >}}
**Register a flag for each key you want settable from the command line.** `config.Flags(fs)` returns
only the flags explicitly set on `fs`, so a custom key such as `APP_JWKS_URL` falls through to the
environment and file sources until you define a corresponding flag on the same `FlagSet`.
{{< /callout >}}

## YAML and TOML support

To read YAML or TOML files, import the `config/koanf` adapter. This adapter is the only package
that imports koanf; the core `config` package stays stdlib-only.

```go
import (
    "github.com/infobloxopen/devedge-sdk/config"
    konfig "github.com/infobloxopen/devedge-sdk/config/koanf"
)

src, err := konfig.YAMLFile("config.yaml")
if err != nil { log.Fatal(err) }
config.Load(&opts, config.Flags(fs), config.Env("SVC_"), src)
```

`YAMLFile` returns a `*KoanfSource` that implements `config.Source` and slots into the standard
precedence chain like any other source. koanf lowercases keys by default; `KoanfSource.Get`
accepts both upper- and lower-case keys.

## Implementing a custom source

Any type with a `Get(key string) (string, bool)` method satisfies `config.Source`:

```go
type VaultSource struct { client *vault.Client }
func (s *VaultSource) Get(key string) (string, bool) {
    secret, err := s.client.KVv2("secret").Get(ctx, key)
    if err != nil { return "", false }
    v, ok := secret.Data["value"].(string)
    return v, ok
}
// Then:
config.Load(&opts, &VaultSource{client: vc}, config.Env("SVC_"), config.DotEnv(".env"))
```

## Stdlib dependency guarantee

The core `config` package imports only Go standard library packages. The `cleancore_test.go`
integration test asserts this at every CI run:

```sh
go list -deps ./config | grep koanf   # must be empty
go list -deps ./config/koanf | grep koanf  # must be non-empty
```

## Scaffold env-var reference

The scaffolded service uses an `<SVC>_` prefix where `<SVC>` is the service name uppercased.
For a service named `orders`:

| Env var | Default | Purpose |
|---|---|---|
| `ORDERS_GRPC_ADDR` | `:9090` | gRPC listen address |
| `ORDERS_HTTP_ADDR` | `:8080` | HTTP gateway address |
| `ORDERS_LOG_LEVEL` | `info` | Minimum log level |
| `ORDERS_OTLP_ENDPOINT` | `""` | OTel collector endpoint |
| `ORDERS_DSN` | `""` | Database connection string |
