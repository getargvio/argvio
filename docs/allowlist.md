# The allowlist / consent-tier schema

`schema/allowlist/v1.yaml` is the single source of truth for two things,
both enforced in `public` before a row is ever written:

1. **Layer 2 semantic validation** — which metric names, span name
   patterns, and log event names we accept at all, and which attributes
   each may carry.
2. **Consent-tier field mapping** — the minimum tier (`anonymous` /
   `basic` / `full` / `optin_plus`) a record must declare for each
   attribute to be retained.

Nothing in `internal/otlp` (the ingest path) hardcodes an event name or
attribute key — everything resolves through `internal/allowlist.Schema`,
loaded from this file at startup and hot-reloadable
(`internal/allowlist/loader.go` watches the file via `fsnotify`; a reload
that fails to parse/validate is logged and discarded, so a bad edit never
takes ingest down or silently opens it up — the previously-loaded schema
keeps serving).

## File shape

```yaml
schema_version: 1
supported_signal_types: [metrics, logs, traces]
standard_resource_attributes: [service.name, telemetry.sdk.name, ...]

consent_tiers:
  - { name: anonymous, rank: 0 }
  - { name: basic,     rank: 1 }
  - { name: full,      rank: 2 }
  - { name: optin_plus, rank: 3 }

attributes:
  cli.command.exit_code:
    type: int
    min_tier: anonymous
    sensitive: false

metrics:
  - name: cli.command.invocations
    type: sum
    unit: "1"
    allowed_attributes: [cli.command.name, cli.command.exit_code, ...]

spans:
  - name_pattern: "cli.command.*"     # trailing "*" wildcard only
    allowed_attributes: [...]

logs:
  - event_name: cli.error.raised
    allowed_attributes: [...]
```

`attributes` is the shared dictionary — every `allowed_attributes` entry
under `metrics`/`spans`/`logs` must reference a key defined here
(`Schema.finalize()` fails fast at load time if not). This is what lets one
attribute (e.g. `cli.version`) be reused across many event types without
redefining its type/bounds/tier each time.

## Enforcement pipeline (`internal/consent.EvaluateEvent`)

For each span / log record / metric data point:

1. **Name lookup.** `MetricByName` / `MatchingSpanDef` / `LogEventByName`.
   Not found → **reject the whole record.** We do not accept arbitrary
   names, per the hard requirement — nothing gets silently
   accepted-and-dropped.
2. **Tier resolution.** `cli.analytics.tier` is read from record-scope
   attributes first, falling back to resource-scope
   (`internal/consent.ResolveTier`). Missing or unrecognized → reject
   (Layer 1: required attribute).
3. **Ceiling capping.** `effective_tier = min(declared_tier, tenant.tier_ceiling)`.
   If declared > ceiling and the tenant's `tier_enforcement_mode` is
   `reject`, the whole record is rejected; if `strip`, it's accepted at
   the capped tier.
4. **Per-attribute filtering.** For every attribute still on the record:
   dropped (not reject-the-record) if it isn't in this event's
   `allowed_attributes` and isn't a `standard_resource_attributes` entry,
   fails its `type`/`max_length`/`enum` check
   (`internal/allowlist.CheckAttribute`), or has a `min_tier` above the
   effective tier. A single malformed or over-scoped field degrades the
   record; it doesn't nuke an otherwise-valid one.

Every drop/reject is logged with the reason and the attribute *key* —
never the value, since the value may be exactly the sensitive thing being
dropped.

## Consent tiers

| Tier | Adds |
|---|---|
| `anonymous` | Aggregate-only shape: `cli.command.name`, `cli.command.exit_code`, `cli.command.duration_ms`, `cli.version`, `cli.os`, `cli.arch`, `cli.is_ci`. |
| `basic` | Opaque `cli.session_id`/`cli.install_id` (random UUIDs, not identity-linked), `cli.flag.names` (names only, not values), shell/terminal metadata. |
| `full` | Error diagnostics (`cli.error.type`/`message`/`stacktrace`), the full allowlisted attribute set for allowed events. |
| `optin_plus` | Fields marked `sensitive: true` explicitly: `cli.args.raw`, `cli.env.vars`, `cli.host.name`, `cli.user.name`, `cli.path.raw`. Only accepted if **both** the tenant's ceiling is `optin_plus` and the record itself declares it. |

## Worked example: adding a new supported log event

Say a tenant's SDK wants to emit `cli.plugin.loaded` with the plugin name
and load duration. Two things to add to `schema/allowlist/v1.yaml`, no Go
code changes:

```yaml
attributes:
  cli.plugin.name:
    type: string
    max_length: 64
    min_tier: basic
    sensitive: false

  cli.plugin.load_duration_ms:
    type: int
    min_tier: basic
    sensitive: false

logs:
  # ...existing entries...
  - event_name: cli.plugin.loaded
    allowed_attributes:
      - cli.plugin.name
      - cli.plugin.load_duration_ms
      - cli.command.name
```

Save the file. If `public.allowlist_hot_reload` is true (default), the
running server picks it up within one `fsnotify` event — no restart, no
deploy. `argvio-admin` doesn't need to know about this either; it's purely
data-driven.

## Non-standard resource attributes

`standard_resource_attributes` (e.g. `service.name`,
`telemetry.sdk.name/language/version`) are always kept at `anonymous` tier
regardless of the per-event `allowed_attributes` lists — these come from
the OTel SDK itself, not CLI-specific instrumentation, and carry no PII.
Anything else not on an event's allowlist is dropped per the rules above.
