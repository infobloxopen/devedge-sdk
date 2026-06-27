# F036 — Unified configuration: env + flags + file behind a neutral, dependency-light seam

**Status**: design locked. **Issue**: #93 (DX 057, P1). **Initiative**: WS-007.
**Depends on**: nothing in code; consumed by the scaffold + #95 deploy (which renders config into env/values).
**Pre-GA**: clean implementation over back-compat.

**Origin / user directive (load-bearing principle)**: "a neutral config seam; env / flags / file
(viper-or-equivalent) loaders are swappable adapters — **no heavy config dep in core**."

## Problem
At v0.23.0 there is no configuration story. The scaffold hardcodes addresses (`:8080`/`:8081`), reads a
couple of env vars ad hoc, and there is no typed, precedence-ordered way to load a service's settings (server
addrs, DSN, log level, OTLP endpoint, feature toggles). A "microservices foundation" must offer a coherent
config story — without dragging a heavy config library (viper) into every consumer's dependency graph.

## Decision (locked)
- **Core `config` package — stdlib only, the seam.**
  ```go
  package config // github.com/infobloxopen/devedge-sdk/config
  type Source interface { Get(key string) (value string, ok bool) }
  // Built-in sources (all stdlib): Env(prefix), Flags(*flag.FlagSet), DotEnv(path), JSONFile(path), Map(m).
  // Load binds tagged struct fields from sources by precedence (earlier source wins), with typed parsing
  // (string,int,int64,bool,float64,time.Duration) and `default:""` fallbacks.
  func Load(dst any, sources ...Source) error   // reflects `config:"KEY"` + `default:"…"` tags
  ```
  Built-in sources use only `os`, `flag`, `strconv`, `time`, `reflect`, `encoding/json`, `bufio` — **no third
  party**. "File" support in core = `.env` (dotenv line parser) + JSON (`encoding/json`).
- **Adapter `config/koanf` — the heavy dep, isolated.** A `KoanfSource` (or provider) implementing `Source`
  over `github.com/knadh/koanf` for YAML/TOML/HCL/remote/watch. The ONLY package importing koanf. A consumer
  who wants YAML opts into this package; core stays stdlib-only. (koanf chosen over viper: smaller graph,
  no global state — but the seam makes the choice swappable regardless.)
- **Canonical `ServerOptions`.** A typed struct the SDK + scaffold load through `config.Load`: `GRPCAddr`,
  `HTTPAddr`, `LogLevel`, `OTLPEndpoint` (feeds #90), `DSN` (feeds persistence + #95 secret), plus room for
  per-service fields. Sane defaults via `default:` tags. The scaffold's `main.go` replaces hardcoded values
  with `config.Load(&opts, config.Flags(fs), config.Env("<SVC>_"), config.DotEnv(".env"))`.

## Locked defaults
- Precedence (highest→lowest): **flags > env > file > struct `default:` tag**.
- Core file formats: `.env` + JSON only (stdlib). YAML/TOML/remote = the koanf adapter.
- No hot-reload / no remote / no secret-manager integration in this feature (scope gate; the seam allows them
  as future sources/adapters — `config/koanf` already supports file-watch, left undocumented-as-supported here).

## Design — files
- `config/config.go` (new): `Source`, `Load` (reflective binder + typed parse + precedence + defaults).
- `config/sources.go` (new): `Env`, `Flags`, `DotEnv`, `JSONFile`, `Map` (all stdlib).
- `config/options.go` (new): canonical `ServerOptions` with `config:`/`default:` tags.
- `config/koanf/koanf.go` (new adapter): `KoanfSource` (YAML/TOML/remote). Sole koanf importer.
- `config/cleancore_test.go` or extend root `cleancore_test.go`: guard — no core package imports koanf.
- Scaffold: `main.go.tmpl` + `main.ent.go.tmpl` load `ServerOptions` via `config.Load`; `README.md.tmpl`
  documents the precedence + env names; `.env`-style example noted.
- Docs: `docs/content/docs/guides/configuration.md`.

## Acceptance criteria
- **AC-1.** `config.Load(&opts, Flags, Env("PFX_"), DotEnv(".env"))` populates a typed struct honoring the
  documented precedence and `default:` tags; typed fields (int/bool/duration) parse correctly; a malformed
  value returns a clear error naming the key.
- **AC-2 (dependency-light gate).** No core package imports koanf (or viper); `config/koanf` is the sole
  importer. Guard test proves it; core `config` import-closure is stdlib-only.
- **AC-3.** The scaffolded service loads `GRPCAddr/HTTPAddr/LogLevel/OTLPEndpoint/DSN` through `config.Load`
  (no hardcoded addresses); overriding via env and via flag both work end-to-end in the smoke test.
- **AC-4.** The koanf adapter loads a YAML file through the same `Source` seam (adapter test).
- **AC-5 (gates).** build/vet/test/security green; scaffold E2E green.

## Failure modes
- **Reflective binder mis-parses / panics on unsupported field types** → fix: explicit supported-kinds switch,
  error (not panic) on unsupported kinds; table-test each kind.
- **Precedence surprises** → document precedence prominently; test all orderings.
- **koanf leaks into core** via the canonical options or scaffold default path → guard test forbids it.

## Tasks
- **T1 [C]** `config/config.go` reflective `Load` (precedence + typed parse + defaults) — the non-trivial core.
- **T2 [S]** `config/sources.go` stdlib sources; `config/options.go` canonical `ServerOptions`.
- **T3 [S]** `config/koanf` adapter + dep-light guard test.
- **T4 [S]** scaffold integration (`main.go.tmpl`/`main.ent.go.tmpl`, README) — replace hardcoded addrs.
- **T5 [S]** tests (precedence, typed parse, error paths) + docs `guides/configuration.md`.

## Exit
All ACs green; PR merged; tag cut. DX cadence shows a unified config story present.
