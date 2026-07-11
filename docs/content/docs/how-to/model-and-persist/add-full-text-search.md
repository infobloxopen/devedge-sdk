---
title: Add full-text search to a resource
weight: 6
---

Full-text search is a `q` query parameter that the storage generators add to a resource's List
operation. You declare which fields — or calculated values, such as an enum mapped to a display
label — participate, and the generator wires the matching query for you: a Postgres full-text
predicate on the generated repository, with a portable fallback for local development.

Use this page when a List endpoint needs "type some words in a box, get matching records" search
across a handful of fields. For an exact-match predicate over a known field and value, use
[`filter`](../../../reference/persistence/#filtering-and-ordering) instead — the two compose (see
[Combine `q` with `filter` and `order_by`](#combine-q-with-filter-and-order_by) below). This guide
assumes you already have a [modeled resource](../model-a-resource/) with an `id` field.

## Prerequisites

- A resource message with an `id` field (see [Model a Resource](../model-a-resource/)).
- The v0.60.0 devedge-sdk toolchain or later. `searchable` and `(infoblox.storage.v1.search)` are
  additive schema options: an older `de`/toolchain parses and generates the resource without
  error, but silently ignores them — no `q` predicate and no migration is emitted. Confirm your
  toolchain is on v0.60.0 or later before following this guide.
- PostgreSQL to exercise true full-text matching. A resource whose sources are all portable also
  runs on the SQLite dev/test driver — see [SQLite and `sql`-flavor sources](#sqlite-and-sql-flavor-sources)
  below.

## 1. Mark a field searchable

Add `searchable: true` to an `(infoblox.field.v1.opts)` on a plain field:

```proto {filename="widgets.proto"}
message Widget {
  string id           = 1;
  // display_name is included in the resource's full-text search vector.
  string display_name = 2 [(infoblox.field.v1.opts) = {searchable: true}];
}
```

A resource can flag more than one field; the generator unions them into one vector. The `User`
fixture below flags two:

```proto {filename="iam.proto"}
message User {
  string id           = 1;
  string account_id   = 2;
  string email        = 3 [(infoblox.field.v1.opts) = {unique: true, searchable: true}];
  string display_name = 4 [(infoblox.field.v1.opts) = {searchable: true}];
}
```

On PostgreSQL, a searchable field's `@` and `.` characters are normalized to spaces before
indexing — `alice@acme.com` tokenizes as `alice`, `acme`, and `com` — so an email or hostname field
matches on its parts, not only on the whole value. This normalization applies to a field-flagged or
`field`-referenced source only; it does not apply to a `sql`/`cel` calculated source, and the
SQLite fallback (below) does not normalize at all.

{{< callout type="warning" >}}
**A `secret` or `INPUT_ONLY` field cannot be searchable.** Matching against a write-only or
redacted value would leak it through match behavior, so `make generate` fails and names the field.
`searchable` also requires a textual type: `string`, `enum`, a string-typed repeated field,
`map<string, string>` tags, or `Timestamp`.
{{< /callout >}}

## 2. Add a calculated source for a value with no single column

A transform — an enum mapped to a display label, a value pulled out of a JSON column — has no
single field to flag. Declare it instead as a **source** in a message-level
`(infoblox.storage.v1.search)` option. A source is either a `field` reference or a set of flavored
expressions (`exprs`).

```proto {filename="widgets.proto"}
import "infoblox/storage/v1/storage.proto";

message Widget {
  option (infoblox.storage.v1.search) = {
    sources: [
      { name: "category_label", exprs: { expr: [ /* see below */ ] } }
    ]
  };
  // ...
}
```

### The `field` source

Reference an existing message field by name. This is equivalent to flagging the field
`searchable` directly, but lets you list it alongside calculated sources or override its
`text_config`:

```proto
sources: [
  { name: "sku", field: "sku", text_config: "simple" }
]
```

### The `sql` source (PostgreSQL)

Write a raw Postgres expression over the resource's own row — the escape hatch for a `CASE`
mapping or a JSON lookup. `dialect` must be `"postgres"`; `version` pins the expression's meaning
for the flavor's validator:

```proto
sources: [
  {
    name: "category_label"
    exprs: {
      expr: [
        {
          flavor: "sql"
          dialect: "postgres"
          version: "1"
          expr: "CASE category WHEN 'premium' THEN 'tier premium deluxe' WHEN 'standard' THEN 'tier standard basic' ELSE 'tier none' END"
        }
      ]
    }
  }
]
```

The expression must read only the current row — no `SELECT`/`FROM`/`JOIN` and no volatile function
such as `now()` or `random()` — because it becomes an immutable, single-row `to_tsvector(...)`
argument (and, for the `INDEXED` strategy, a generated column expression).

### The `cel` source (portable)

A CEL expression compiles to an equivalent SQL fragment for **both** Postgres and the SQLite
fallback, so a calculated value stays portable without you hand-writing two expressions. It
supports field references, a ternary/map-literal lookup compiled to `CASE`, and basic string
operations:

```proto
{
  flavor: "cel"
  version: "1"
  expr: "msg.category == 'premium' ? 'tier premium' : 'tier standard'"
}
```

Add a `cel` expression alongside a `sql` one in the same source to get the best of both: the
hand-written Postgres expression is used on Postgres, and the `cel`-compiled expression keeps the
source's SQLite fallback working:

```proto
sources: [
  {
    name: "category_label"
    exprs: {
      expr: [
        { flavor: "sql", dialect: "postgres", version: "1",
          expr: "CASE category WHEN 'premium' THEN 'tier premium deluxe' WHEN 'standard' THEN 'tier standard basic' ELSE 'tier none' END" },
        { flavor: "cel", version: "1",
          expr: "msg.category == 'premium' ? 'tier premium' : 'tier standard'" }
      ]
    }
  }
]
```

See [Annotations → Full-text search](../../../concepts/annotations/#full-text-search) for the full
field-by-field reference of `SearchConfig`, `SearchSource`, and `SearchExpr`.

## 3. Choose a strategy: `JIT` or `INDEXED`

The message-level `search {}` option also selects how the search vector is materialized, via
`strategy`:

| Strategy | Behavior | Use when |
|---|---|---|
| `STRATEGY_JIT` (default) | Recomputes `to_tsvector(...)` on every query. No migration. | Development, and tables small enough that a query-time scan stays fast. |
| `STRATEGY_INDEXED` | Persists a generated `search_vector` column and a GIN index; `q` matches the indexed column. | Tables where a query-time `to_tsvector` scan is too slow — a query-time-only full-text predicate has been measured at 5–30 seconds per query on a large production table. |

```proto {filename="widgets.proto"}
message Gizmo {
  option (infoblox.storage.v1.search) = {
    strategy: STRATEGY_INDEXED
    sources: [ /* ... */ ]
  };
  string id    = 1;
  string label = 2 [(infoblox.field.v1.opts) = {searchable: true}];
}
```

{{< callout type="info" >}}
A third strategy, `STRATEGY_PROJECTED`, appears in the schema but has no local implementation:
declaring it is valid to parse, but `make generate` fails loud rather than silently falling back to
a local predicate. It is reserved for a future cross-service search index.
{{< /callout >}}

## 4. Regenerate and, for INDEXED, apply the migration

```sh
make generate
```

For a `STRATEGY_INDEXED` resource, `make generate` also emits a migration pair: an
`ALTER TABLE ... ADD COLUMN search_vector ... GENERATED ALWAYS AS (...) STORED` and a
`CREATE INDEX CONCURRENTLY ... USING GIN (search_vector)` in its own file (Postgres cannot run
`CONCURRENTLY` inside a transaction).

{{< callout type="warning" >}}
**The generated files land under `gen/migrations/`, not the module's committed migrations.**
`gen/migrations/` is git-ignored and is not the directory the module embeds. The scaffold's
`make sync-migrations` target — wired as a prerequisite of `make build`, `make test`, and
`make run` — copies the generated files into the committed, embedded `module/migrations/`
directory, where `//go:embed migrations` and the host migrator can find them. If you invoke
`de generate` directly instead of through `make`, run `make sync-migrations` (or `make build`)
afterward. Skip this step and an `INDEXED` resource fails at runtime with
`column "search_vector" does not exist`.
{{< /callout >}}

Once synced, review and apply the files the same way as any other migration:

```sh
de migrate lint
de migrate up --dsn postgres://…/mydb
```

See [Change the database schema](../change-the-database-schema/) for the general migration
workflow, and [Persistence reference → Full-text search migrations](../../../reference/persistence/#full-text-search-migrations-indexed)
for the exact generated file shape.

## 5. Query with `q`

`q` is a plain `string` field on the List request, following the same convention as `filter` and
`order_by`:

```proto {filename="widgets.proto"}
message ListWidgetsRequest {
  int32  page_size  = 1;
  string page_token = 2;
  string q          = 5;
}
```

`protoc-gen-svc` maps it onto `persistence.ListOptions.Search`, so no handler code is required.
Call it as a query parameter over the REST gateway:

```sh
curl -s 'localhost:8080/v1/widgets?q=acme'
```

An empty or whitespace-only `q` is a no-op — List behaves exactly as if no search were requested
and returns every row that the other List parameters allow.

## Combine `q` with `filter` and `order_by`

`q` is its own operator: it ANDs onto the existing `filter` predicate rather than being folded into
the AIP-160 grammar, and composes with `order_by` and pagination in the same List call:

```sh
curl -s 'localhost:8080/v1/widgets?q=acme&filter=category="premium"&order_by=create_time+desc'
```

This returns only rows that match the free-text query **and** the filter, ordered by
`create_time`. No operator is dropped when you combine all three.

{{< callout type="warning" >}}
**`q`, `filter`, and `order_by` are three independent, opt-in request fields.** `protoc-gen-svc`
wires each one only when the List request message declares a same-named `string` field — adding
`q` does not automatically add `filter` or `order_by`, and vice versa. To compose all three, the
request must declare all three:

```proto {filename="widgets.proto"}
message ListWidgetsRequest {
  int32  page_size  = 1;
  string page_token = 2;
  string filter     = 4;
  string order_by   = 6;
  string q          = 5;
}
```
{{< /callout >}}

## SQLite and `sql`-flavor sources

A resource whose sources are all portable — plain `field` sources, or a `cel` source, which
compiles to a SQLite expression as well as a Postgres one — also works on the SQLite driver used
for fast local tests. There, `q` degrades to a single case-insensitive `LIKE` contains: every
portable source is concatenated into **one string** with a literal space between sources, and the
whole concatenation is matched against the term. This is not a per-column `OR` — a match is not
required to fall within a single source; it can span the boundary between two concatenated
sources.

{{< callout type="warning" >}}
**The SQLite fallback is a dev-only approximation, not full-text search.** Its matching semantics
differ materially from PostgreSQL's: no stopword removal, no stemming, matches occur mid-word (a
substring match, not a token match), and a query can match across the seam between two
concatenated sources. A search that matches on SQLite — or the order it returns — can differ from
the same search on PostgreSQL, and vice versa. Validate search behavior against PostgreSQL before
deploying; do not treat a SQLite result set as a preview of production search results.
{{< /callout >}}

{{< callout type="warning" >}}
**A `sql/postgres` source with no `cel` alternate is Postgres-only.** This is a runtime
constraint, not a build-time one: the resource generates without error on every backend. When List
runs against a non-Postgres connection, the generated predicate returns a gRPC `Unimplemented`
error — "full-text search for `<Resource>` requires PostgreSQL" — instead of running broken SQL or
silently matching zero rows. Both backends fail loud identically; see
[Persistence reference → Full-text search (`q`)](../../../reference/persistence/#full-text-search-q)
for the exact predicate per backend. Add a `cel` expression alongside the `sql` one (as shown in
[The `cel` source](#the-cel-source-portable) above) to keep the resource portable.
{{< /callout >}}

## See also

- [Annotations → Full-text search](../../../concepts/annotations/#full-text-search) — the complete
  `searchable` / `SearchConfig` / `SearchSource` / `SearchExpr` field reference.
- [Persistence reference → Full-text search (`q`)](../../../reference/persistence/#full-text-search-q) —
  `ListOptions.Search`, the generated predicate per backend and dialect, and the generated INDEXED
  migration files.
- [Model a Resource](../model-a-resource/) — field types and annotations in general.
- [Change the database schema](../change-the-database-schema/) — the general migration workflow.
