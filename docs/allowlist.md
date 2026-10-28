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
  cli.error.reason:
    type: string
    max_length: 64
    pattern: "^[a-z0-9][a-z0-9._-]*$"   # optional, string only; anchor it
    min_tier: basic
    sensitive: false

metrics:
  - name: cli.command.invocations
    type: sum
    unit: "1"
    allowed_attributes: [cli.command.name, cli.command.exit_code, ...]
  - name: cli.counter
    type: sum
    temporality: delta                 # optional, sum only: reject other temporalities
    required_attributes: [cli.counter.name]  # optional: reject points that lose these
    allowed_attributes: [cli.counter.name, cli.counter.label, ...]
  - name: cli.measure
    type: histogram
    unit: ms
    temporality: delta                 # optional, sum and histogram: reject other temporalities
    explicit_bounds: [1, 5, 10, ...]   # optional, histogram only: the only bucket bounds accepted
    allowed_attributes: [cli.measure.name, cli.measure.label, ...]

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
   fails its `type`/`max_length`/`enum`/`pattern` check
   (`internal/allowlist.CheckAttribute`), or has a `min_tier` above the
   effective tier. A single malformed or over-scoped field degrades the
   record; it doesn't nuke an otherwise-valid one.
5. **Required attributes.** A metric data point that no longer carries one
   of its metric's `required_attributes` after step 4 is rejected: it would
   mean nothing. Before any of this, a metric with a declared `temporality`
   is rejected whole if it was sent with another one, and a `monotonic`
   sum's data point is rejected if its value is negative or not finite. A
   metric with declared `explicit_bounds` is rejected whole if it isn't in the
   declared `unit`, and a data point of it is rejected if its bucket bounds
   differ, its bucket counts don't add up to its count, or its sum, min or max
   is negative or not finite.

Every drop/reject is logged with the reason and the attribute *key* —
never the value, since the value may be exactly the sensitive thing being
dropped.

## Consent tiers

| Tier | Adds |
|---|---|
| `anonymous` | Aggregate-only shape: `cli.command.name`, `cli.command.exit_code`, `cli.command.duration_ms`, `cli.version`, `cli.os`, `cli.arch`, `cli.is_ci`, vendor counters (`cli.counter.name`, `cli.counter.label`; **see the warning under "Vendor counters"**) and vendor durations (`cli.measure.name`, `cli.measure.label`; **see "Vendor measures"**). |
| `basic` | Opaque `cli.session_id`/`cli.install_id` (random UUIDs, not identity-linked), `cli.flag.names` (names only, not values), shell/terminal metadata, the coarse `cli.error.type` category and the vendor's finer `cli.error.reason` (see below), and process resource usage on the command span (`cli.process.peak_rss_bytes`, `cli.process.cpu_time_ms`), and build footprint on `cli.session.started` (`cli.binary.size_bytes`, `cli.startup.duration_ms`), and the environment of a session on `cli.session.started` (`cli.shell`, `cli.terminal`, `cli.stdout.is_tty`, `cli.runtime.name`/`version`, `cli.config.customized_count`, `cli.auth.state`; `GET /v1/metrics/session-environment`)., and the nearest known command to a mistyped one on `cli.command.unknown` (`cli.command.suggestion`; see "Unknown commands"; the event itself is accepted at `anonymous` as a bare count). |
| `full` | Error diagnostics (`cli.error.message`/`stacktrace`), the full allowlisted attribute set for allowed events. |
| `optin_plus` | Fields marked `sensitive: true` explicitly: `cli.args.raw`, `cli.env.vars`, `cli.host.name`, `cli.user.name`, `cli.path.raw`. Only accepted if **both** the tenant's ceiling is `optin_plus` and the record itself declares it. |

## Error reasons (`cli.error.reason`)

`cli.error.type` is a coarse category (network, timeout, ...). A vendor with
its own fixed list of failure causes ("no token", "invalid url", "failed to
read config file") sends one of them as `cli.error.reason`, on the
`cli.errors` metric and the `cli.error.raised` event, next to the category.
It is `basic` tier because it is meant to be an identifier defined in the
CLI's code, never the error text.

**The SDK does not scrub the value**: it sends whatever the CLI passes. Two
guards on the server keep a vendor bug (say, putting the error message or
user input in the field) from leaking free text or blowing up storage:

1. **Format.** The schema `pattern` `^[a-z0-9][a-z0-9._-]*$`, with
   `max_length: 64`: lowercase letters, digits, `.`, `_` and `-`, starting
   with a letter or digit. A value that doesn't match is dropped like any
   other invalid attribute, and the record is kept without it. No spaces,
   slashes, quotes or uppercase get through, so paths and sentences can't.
2. **Cardinality.** Each tenant has a vocabulary of at most
   `public.max_error_reasons_per_tenant` distinct reasons (default 100),
   kept in the `error_reasons` table. A reason already in it is stored
   as-is; a new one is admitted while there is room (within a batch, most
   frequent first, then by name), and otherwise stored as `other`. Folded
   records are counted per tenant in `error_reason_overflow`
   (`folded_count`, `last_folded_at`) and logged as a warning. `other` is
   reserved: always accepted, never counted toward the cap, so a vendor
   that sends it literally mixes with folded values. Reasons not seen for
   the tenant's logs retention window are dropped by `argvio retention
   sweep`, which frees their slots.

The cap is applied in the ingest pipeline after allowlist filtering and
before the rows are written (`internal/otlp/reasons.go`). Records without
the attribute (older SDKs) are unaffected.

`GET /v1/metrics/error-types?group_by=reason` breaks each category down by
reason, per command; without `group_by` the response is unchanged. Like the
category, it is read from `cli.error.raised` events, so it only counts what
the SDK sent as events. No continuous aggregate reads `cli.errors` from the
`metrics` table, so carrying the reason on it needed no rollup change or
backfill.

## Vendor counters (`cli.counter`)

> **Warning: counters are accepted at the `anonymous` tier.** They are
> reported from CI and from users who allow no install identifier, under the
> lowest consent level there is. `cli.counter.name` and `cli.counter.label`
> must be **fixed identifiers chosen in the CLI's code**: never user input,
> file paths, host names, URLs, tokens, or anything derived from them. The SDK
> does not scrub them.

`cli.counter` is one generic counter for things specific to a vendor's
product ("OpenAPI security schemes seen, by type", "rules generated, by HTTP
verb"), so a new one needs no server change. It is a `sum`, `monotonic`,
unit `1`, and must be sent with **delta** temporality: each data point adds
its value to the counter named by `cli.counter.name`, optionally broken down
by `cli.counter.label`. It may also carry `cli.command.name`, `cli.version`,
`cli.os`, `cli.arch` and `cli.is_ci`, and nothing that identifies an install
or a session.

What the server enforces:

1. **Shape.** A `cli.counter` metric sent with cumulative temporality is
   rejected whole (summing cumulative points would double count), and a data
   point with a negative or non-finite value is rejected.
2. **Format.** Name and label have the same `pattern` and `max_length: 64`
   as `cli.error.reason`: `^[a-z0-9][a-z0-9._-]*$`. A label that fails it is
   dropped (the point is kept, unlabelled); a name that fails it, or is
   missing, rejects the point, since `cli.counter.name` is a
   `required_attributes` entry.
3. **Cardinality**, per tenant, in the `counter_names` and `counter_labels`
   tables: at most `public.max_counter_names_per_tenant` distinct names
   (default 200) and `public.max_counter_labels_per_name` distinct labels per
   name (default 50). A value already in the vocabulary is stored as-is; a new
   one is admitted while there is room (within a batch, most frequent first,
   then by name). Past the name cap the point is stored under the name
   `other` without a label; past a label cap it keeps its name and its label
   becomes `other`. A name sent as `other` literally also loses its label.
   Folded points are counted per tenant in `counter_overflow`
   (`folded_names`, `folded_labels`, `last_folded_at`), logged as a warning,
   and reported by the API. Values not seen for the tenant's logs retention
   window are dropped by `argvio retention sweep`, freeing their slots.

The caps are server config rather than schema: they bound storage per tenant,
like `public.max_error_reasons_per_tenant`, and changing them needs no
schema reload. They are applied in the ingest pipeline after allowlist
filtering and before the rows are written (`internal/otlp/counters.go`).

`GET /v1/metrics/counters` lists the counter names with their totals over a
window and the fold counts; with `name=` it returns that counter's time
series (hour/day/week/month) and its top labels. Both read the
`cagg_counters_hourly` / `cagg_counters_daily` rollups (docs/schema.md). No
per-install distinct count is offered: the points carry no install id, by
design.

## Vendor measures (`cli.measure`)

> **Warning: measures are accepted at the `anonymous` tier**, exactly like
> counters (see the warning under "Vendor counters"). `cli.measure.name` and
> `cli.measure.label` must be **fixed identifiers chosen in the CLI's code**:
> never user input, file paths, host names, URLs, tokens, or anything derived
> from them. The SDK does not scrub them.

`cli.measure` is the counterpart of `cli.counter` for durations: one generic
instrument for how long things specific to a vendor's product take (external
tools, network probes, scans), so a new one needs no server change. It is a
`histogram`, unit `ms`, and must be sent with **delta** temporality: each data
point holds the durations recorded since the last export for the measure named
by `cli.measure.name`, optionally broken down by `cli.measure.label`. It may
also carry `cli.command.name`, `cli.version`, `cli.os`, `cli.arch` and
`cli.is_ci`, and nothing that identifies an install or a session. Compose a
label such as `hmac-sha256` to correlate several dimensions within the
64-byte / per-name label limits; arbitrary numeric attributes are not
supported.

The buckets are fixed by the server so that SDKs in different languages
agree and their counts can be added: `explicit_bounds` in the schema, in
milliseconds,

    1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 60000, 300000

that is 12 buckets, `(-inf, 1]` to `(300000, +inf)`. SDKs configure their
histogram view with exactly these.

What the server enforces:

1. **Shape.** A `cli.measure` metric that is not a histogram in `ms` with
   delta temporality is rejected whole. A data point whose bucket bounds
   differ from the above, whose bucket counts don't add up to its count, or
   whose sum, min or max is negative or not finite, is rejected.
2. **Format.** Name and label have the same `pattern` and `max_length: 64` as
   counters: `^[a-z0-9][a-z0-9._-]*$`. A label that fails it is dropped (the
   point is kept, unlabelled); a name that fails it, or is missing, rejects
   the point, since `cli.measure.name` is a `required_attributes` entry.
3. **Cardinality**, per tenant, in the `measure_names` and `measure_labels`
   tables: at most `public.max_measure_names_per_tenant` distinct names
   (default 100) and `public.max_measure_labels_per_name` distinct labels per
   name (default 50), independent of the counter caps. Folding works as for
   counters: past the name cap the point is stored under the name `other`
   without a label, past a label cap its label becomes `other`, folded points
   are counted in `measure_overflow` (`folded_names`, `folded_labels`,
   `last_folded_at`), logged as a warning and reported by the API, and
   `argvio retention sweep` frees the slots of values no longer sent. The cap
   is applied in the ingest pipeline after allowlist filtering and before the
   rows are written (`internal/otlp/measures.go`).

`GET /v1/metrics/measures` lists the measure names with their count, sum,
min, max and p50/p95/p99 over a window, and the fold counts; with `name=` it
returns that measure's series (hour/day/week/month) and the same statistics
per label, all narrowable by version, OS, arch, command and CI. Both read the
`cagg_measures_hourly` / `cagg_measures_daily` rollups (docs/schema.md). The
percentiles are **estimates**: the rollups keep the summed count of each
bucket, and the percentile is interpolated inside the bucket it falls in
(`internal/storage/measure_percentiles.go`), so they are only as precise as
the bucket is narrow (a p95 of 700 ms means somewhere in 500-1000 ms) and
clamped to the observed min and max. Changing the bounds takes a new
migration, since the rollups have one column per bucket.

## Unknown commands (`cli.command.unknown`)

> **Warning: a mistyped command is arbitrary user input.** It can be a typo of
> a command, but also a pasted token, a path, or a sentence. **Never send what
> the user typed, or any part or transformation of it** (a prefix, a hash, a
> length, the flags that followed it). The server cannot tell a command name
> from typed text, so keeping it out is the SDK's job; the server's guards below
> are a second line, not a licence.

`cli.command.unknown` is the log event for one attempt to run a command that
does not exist. It is for the SDK or the CLI's command framework to emit when
its own dispatcher finds no match, and it has no counterpart in the
`cli.command.*` spans or in `cli.command.invocations`: an attempt is not an
invocation.

**What to send**, and nothing else:

| Attribute | Tier | Rule |
|---|---|---|
| `cli.command.suggestion` | `basic` | Optional. The nearest **known** command name, taken from the CLI's own command table (e.g. `deploy.push`), at most 128 bytes. Omit it when no command is close enough. |
| `cli.version`, `cli.os`, `cli.arch`, `cli.is_ci` | `anonymous` | As on other events. |
| `cli.analytics.tier` | | As on every record. |

**What not to send.** `cli.command.name` is not allowed on this event (it is
the name of a command that ran), nor are `cli.flag.names`, `cli.args.raw`,
`cli.help.flag` or any error attribute: the server drops them. Leave the OTLP
log body empty. The event is marked `drop_body: true` in the schema, so the
body is discarded at every tier, `full` and `optin_plus` included, where other
events keep it.

**Anonymous tier is the counter.** At `anonymous` the suggestion is stripped
(it is `basic`), and what remains is a bare attempt, which still counts toward
the attempt rate. There is no separate counter metric: emit the event at the
consent tier the user is at. A CLI that cannot compute a suggestion emits it
without one.

What the server enforces:

1. **Allowlist and tier.** Only the attributes above are kept. The
   suggestion is stripped below `basic` and dropped when over 128 bytes (the
   attempt is kept). The body is always dropped.
2. **Results never contain a suggestion the tenant has not reported.** The
   suggestion is stored as sent, so a client that puts typed text in it gets it
   stored (in `log_attributes`, like any attribute) but never returned.
   `GET /v1/metrics/unknown-commands` lists a suggestion only when it equals a
   `command_name` the tenant has reported, the set behind
   `GET /v1/meta/dimensions`; every other one is counted in `other` as a number
   of attempts and of distinct suggestions, never as text. Known commands past
   the `limit` fold into `other` too, so the response is bounded.
3. **Rate.** The endpoint reports attempts per bucket next to the command
   invocations of the same bucket (the command stats rollups), and their ratio.

Because the second guard compares with commands the tenant has reported, make
suggestions the same strings as `cli.command.name` (the same dotted path), or
they are folded. See `docs/journey-telemetry-design.md` "Unknown commands" for
the reasoning.

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
deploy. `argvio` doesn't need to know about this either; it's purely
data-driven.

## Non-standard resource attributes

`standard_resource_attributes` (e.g. `service.name`,
`telemetry.sdk.name/language/version`) are always kept at `anonymous` tier
regardless of the per-event `allowed_attributes` lists — these come from
the OTel SDK itself, not CLI-specific instrumentation, and carry no PII.
Anything else not on an event's allowlist is dropped per the rules above.
