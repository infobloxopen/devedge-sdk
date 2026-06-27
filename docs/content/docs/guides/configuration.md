---
title: "Configuration"
weight: 20
---

# Configuration

The SDK ships a dependency-light `config` package that loads service settings from flags,
environment variables, `.env` files, and JSON files through a uniform seam. The core package
uses **stdlib only** — no third-party config library enters a consumer's dependency graph unless
they explicitly opt into the `config/koanf` adapter.

## Precedence (highest → lowest)

```
flags  >  environment  >  file (.env / JSON)  >  default tag
```

The first source that returns a non-empty value for a key wins; subsequent sources are not
consulted for that key.

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

The scaffolded `server/main.go` does exactly this — the addresses, log level, OTLP endpoint, and
DSN come from the config seam, not hardcoded strings.

## Built-in sources (stdlib-only)

| Constructor | Reads from |
|---|---|
| `config.Env(prefix)` | `os.LookupEnv(prefix + key)` |
| `config.Flags(fs)` | `*flag.FlagSet` — only flags explicitly _set_ on the command line (not defaults) |
| `config.DotEnv(path)` | `.env` file at path (`KEY=VALUE` or `KEY="VALUE"` lines; missing file silently empty) |
| `config.JSONFile(path)` | Flat JSON object at path (missing file silently empty) |
| `config.Map(m)` | In-memory `map[string]string` (useful for tests) |

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
An unsupported field kind returns an error (never panics). A malformed value returns a descriptive
error naming the key (e.g. `config: key "WORKERS": cannot parse "bad" as int`).

Fields without a `config:` tag are silently skipped. Embedded structs are flattened recursively.

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

`OTLPEndpoint` feeds `otel.Setup(...OTLPEndpoint...)` in the scaffold — when empty, the standard
`OTEL_EXPORTER_OTLP_ENDPOINT` environment variable is honoured (OTel convention). `DSN` is the
database connection string; empty means the service falls back to its built-in default (in-memory
SQLite in the scaffold).

## YAML / TOML support — the koanf adapter

For YAML or TOML files, opt into the `config/koanf` adapter (the **only** package that imports
koanf — the core `config` package stays stdlib-only):

```go
import (
    "github.com/infobloxopen/devedge-sdk/config"
    konfig "github.com/infobloxopen/devedge-sdk/config/koanf"
)

src, err := konfig.YAMLFile("config.yaml")
if err != nil { log.Fatal(err) }
config.Load(&opts, config.Flags(fs), config.Env("SVC_"), src)
```

`YAMLFile` returns a `*KoanfSource` that implements `config.Source` — it slots into the standard
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

## Dep-light guarantee

The core `config` package imports only stdlib packages (`reflect`, `strconv`, `time`, `os`, `flag`,
`encoding/json`, `bufio`). The `cleancore_test.go` integration test asserts this at every CI run:

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
