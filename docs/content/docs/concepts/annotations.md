---
title: Annotations
weight: 2
---

devedge-sdk's contract is two proto annotations. They are **engine-neutral**: they name *what*
is required, not *how* it is evaluated, so they carry no policy-engine-specific fields.

The annotations come from `infoblox/authz/v1/authz.proto`. The canonical schema lives in the
[`infobloxopen/apis`](https://github.com/infobloxopen/apis) module; the SDK depends on its
generated Go bindings (`github.com/infobloxopen/apis/proto/infoblox/authz/v1`).

## `(infoblox.authz.v1.rule)` — method authorization

Attach a `Rule` to an RPC to declare its authorization requirement:

```proto
service ZoneService {
  rpc GetZone(GetZoneRequest) returns (Zone) {
    option (infoblox.authz.v1.rule) = {verb: "get", resource: "zone:{zone_id}"};
  }
  rpc CreateZone(CreateZoneRequest) returns (Zone) {
    option (infoblox.authz.v1.rule) = {verb: "create", resource: "zone"};
  }
  rpc HealthCheck(HealthRequest) returns (HealthResponse) {
    option (infoblox.authz.v1.rule) = {public: true}; // explicit, auditable opt-out
  }
}
```

The `Rule` message has three fields:

| Field | Number | Meaning |
|---|---|---|
| `verb` | 1 | the canonical permission verb. Standard set: `get`, `list`, `watch`, `create`, `update`, `delete`. Custom verbs (e.g. `download`) are allowed as free strings. `read` is intentionally *not* canonical — it maps to the **View** group (`get` + `list` + `watch`). |
| `resource` | 2 | the resource type or a template over request fields, e.g. `zone` or `zone:{zone_id}`. |
| `public` | 3 | if true, the method requires no authorization. **A method with neither a verb nor `public: true` is denied at runtime** and fails the boot-time completeness gate. |

Each annotation becomes one `authz.MethodRule` in Go:

```go
type MethodRule struct {
    Method   string // transport method id, e.g. "/dns.v1.ZoneService/GetZone"
    Verb     Verb   // the required verb; empty iff Public
    Resource string // resource type or template, e.g. "zone" or "zone:{zone_id}"
    Public   bool   // explicit no-authorization opt-out
}
```

### Two ways to consume it — pick per service

Both produce **identical** `[]authz.MethodRule`:

- **Reflection** — `authzpb.RulesFromGlobal()` reads the annotation off the linked descriptors
  at startup. No generated file.
- **Codegen** — `protoc-gen-devedge-authz` (run by `buf generate`) emits a
  `<Service>AuthzRules` table next to the `.pb.go`. Pass it to `server.Config.Rules` or
  `grpcauthz.WithRules(...)`.

That single set feeds **both** enforcement (the interceptor's rule table) **and** the permission
catalog (`catalog.Build`), which renders per-resource verbs, the endpoints implementing each, and
the View/Manage intent groups.

## `(infoblox.authz.v1.field).secret` — secret fields

Attach a `FieldRule` to a message field to mark it sensitive:

```proto
message APIKey {
  string id         = 1;
  string name       = 2;
  string account_id = 3;
  // key_value is raw API key material. Hashed for lookup, encrypted for recovery,
  // never stored as plaintext, never returned after creation.
  string key_value  = 4 [(infoblox.authz.v1.field).secret = true];
  string key_prefix = 5; // first 8 chars, for display — NOT secret
}
```

`FieldRule` has one field:

| Field | Number | Meaning |
|---|---|---|
| `secret` | 1 | if true, the field contains sensitive data. The framework will: **encrypt/hash at rest** (never store plaintext), **redact in logs** (`[REDACTED]`), and **catch leaks** — the security-check tooling flags any code path that returns the raw value. |

A `secret` field drives behavior across three packages:

- **Storage** (`protoc-gen-storage` / `protoc-gen-ent`) emits `<field>_hash` and
  `<field>_cipher` columns instead of a plaintext column, and calls the `Encryptor` on
  create/update. See [Secret fields](../../guides/secret-fields/).
- **Logging** (`middleware/redact`) replaces the value with `[REDACTED]` before logging.
- **Security** (`seccheck.AssertNoSecretFieldsLeaked`) walks every response proto and fails if a
  secret field is non-empty (other than the literal `[REDACTED]`).

## Extension numbers

The annotations are protobuf custom options:

```proto
extend google.protobuf.MethodOptions {
  Rule rule = 50001;
}
extend google.protobuf.FieldOptions {
  FieldRule field = 50002;
}
```

Both numbers (`50001`, `50002`) are in the protobuf **50000–99999 "internal use"** range. Before
any cross-org publication, obtain a globally-unique number from the protobuf registry.

{{< callout type="warning" >}}
The copy of `authz.proto` checked into the SDK repo is a **mirror** for codegen input only — its
`go_package` points at the canonical `infobloxopen/apis` module, so no Go is generated from the
local copy. Keep it byte-identical to the canonical file.
{{< /callout >}}
