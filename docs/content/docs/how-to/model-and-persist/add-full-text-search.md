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

For a `STRATEGY_INDEXED` resource, `make generate` also writes a migration pair under the module's
`migrations/` directory: an `ALTER TABLE ... ADD COLUMN search_vector ... GENERATED ALWAYS AS (...) STORED`
and a `CREATE INDEX CONCURRENTLY ... USING GIN (search_vector)` in its own file (Postgres cannot run
`CONCURRENTLY` inside a transaction). Review and apply them the same way as any other migration:

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

An empty `q` is a no-op — List behaves exactly as if no search were requested.

## Combine `q` with `filter` and `order_by`

`q` is its own operator: it ANDs onto the existing `filter` predicate rather than being folded into
the AIP-160 grammar, and composes with `order_by` and pagination in the same List call:

```sh
curl -s 'localhost:8080/v1/widgets?q=acme&filter=category="premium"&order_by=create_time+desc'
```

This returns only rows that match the free-text query **and** the filter, ordered by
`create_time`. No operator is dropped when you combine all three.

## SQLite and `sql`-flavor sources

A resource whose sources are all portable — plain `field` sources, or a `cel` source, which
compiles to a SQLite expression as well as a Postgres one — also works on the SQLite driver used
for fast local tests. There, `q` degrades to a case-insensitive `LIKE '%term%'` contains, ORed
across the portable columns, instead of a true full-text match.

{{< callout type="warning" >}}
**A `sql/postgres` source with no `cel` alternate is Postgres-only.** `make generate` fails loud
when generating that resource for the SQLite backend, instead of emitting Postgres-only SQL that
would crash at runtime. Add a `cel` expression alongside the `sql` one (as shown in
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
