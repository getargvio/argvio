# Metric catalog (`argvio.metrics.yaml`)

A CLI's product metrics (`cli.counter`, `cli.measure`, and later `cli.value`)
arrive as names like `vulns.found`. The catalog says what each one **means**:
its title, description, unit, dimensions, whether rising is good news, the
order its dimension values read in, and, for a ratio, how it is computed.
Dashboards render from it instead of from raw names. See
[product-metrics-design.md](product-metrics-design.md) (F3) for the design.

**The file is the only source of truth.** It lives in the CLI's repository,
next to the code that sends the metrics. Applying it *replaces* that CLI's
catalog. A metric that left the file is marked deprecated, not deleted, and
nothing edits a definition field by field: the metrics API serves the
catalog read-only, and its one write takes the whole file (see "Syncing
from CI").

## The file

```yaml
# argvio.metrics.yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/getargvio/argvio/main/schema/metrics/v1.schema.json
version: 1
service: vulnapi            # the CLI's name, as its SDK sends it (service.name)
metrics:
  - name: vulns.found
    kind: counter
    title: Vulnerabilities found
    description: Findings reported by a scan, after deduplication.
    dims: [severity, check]
    direction: up_is_good
    dim_order:
      severity: [critical, high, medium, low, info]
  - name: scan.run
    kind: duration
    title: Scan time
    direction: down_is_good
  - name: vulns.per_scan
    kind: ratio
    title: Vulnerabilities per scan
    numerator:   { metric: vulns.found }
    denominator: { builtin: command_invocations, where: { command: scan } }
```

A tenant may ship several CLIs; each has its own file and catalog, keyed by
`service`. It must match the `service.name` the SDK sends, which both SDKs
set to the CLI name passed to them.

| Field | Applies to | Meaning |
|---|---|---|
| `name` | all | The name the CLI sends (`^[a-z0-9][a-z0-9._-]*$`, at most 64), unique in the file. |
| `kind` | all | `counter` (`cli.counter`), `duration` (`cli.measure`), `value` (`cli.value`) or `ratio`. |
| `title` | all | Required, at most 100 characters. |
| `description` | all | Optional, at most 1000 characters. |
| `unit` | all | `1`, `By`, `%` or `ms`. Defaults to `1`, or `ms` for a duration (which must be `ms`). A value can't be `ms`; time a duration instead. |
| `dims` | counter, duration, value | The dimension keys the CLI sends (`cli.dim.<key>`), at most 8. Ingest still enforces the tenant's `max_metric_dims`. |
| `direction` | all | `up_is_good`, `down_is_good` or `neutral` (default), for colouring changes. |
| `dim_order` | counter, duration, value | Display order of a dimension's values; values not listed come after. Keys must be in `dims`. |
| `min_tier` | all | `anonymous`, `basic`, `full` or `optin_plus`: the lowest consent tier the metric is meant for. |
| `numerator`, `denominator` | ratio | Required. Each is `{metric: <name>}` (a non-ratio metric of the file, optionally `where: {<dim>: <value>}` on its declared dims, and `agg: sum\|count` for a value) or `{builtin: command_invocations\|sessions\|active_installs}` (`command_invocations` may take `where: {command: <name>}`). |
| `scale` | ratio | Multiplier, default 1 (100 gives "per 100"). |

Unknown fields are errors, so a typo can't silently drop a setting.
`schema/metrics/v1.schema.json` gives editors completion and checks.
`argvio metric validate` is the authoritative check: it also checks references
between metrics, and lists every problem at once.

## Ratios

A `kind: ratio` entry is computed when it is read, from two other series,
with the same filters and buckets on both sides (`GET /v1/metrics/ratios`):

| Operand | Reads |
|---|---|
| `{metric: <counter>}` | its total, narrowed by `where` on its dims |
| `{metric: <duration>}` | how many durations were recorded |
| `{metric: <value>, agg: sum}` | the sum of its values (default) |
| `{metric: <value>, agg: count}` | how many runs recorded it |
| `{builtin: command_invocations, where: {command: scan}}` | command runs, optionally of one command |
| `{builtin: sessions}` / `{builtin: active_installs}` | sessions started / distinct installs (basic tier and above), daily only |

Metric operands are scoped to the CLI. **Built-in operands cover the whole
tenant**, so with several CLIs, prefer `command_invocations` with a command,
or a metric of the CLI, as the denominator. A ratio is `null` where its
denominator is 0, never 0.

## Commands

```sh
# Check a file offline (CI, pre-commit).
argvio metric validate -f argvio.metrics.yaml

# Make a tenant's catalog for the file's CLI exactly the file.
argvio metric apply -f argvio.metrics.yaml --tenant-id <TENANT_ID> --dsn "$DSN"
# applied vulnapi to tenant ...: 1 created, 1 updated, 4 unchanged, 1 deprecated

# What the tenant has: declared, deprecated, and sent but undeclared.
argvio metric list --tenant-id <TENANT_ID> [--service vulnapi] [--json]
```

`apply` runs in one transaction, under a lock per (tenant, CLI):

- a metric new to the catalog is created;
- a changed one, or one that was deprecated, is updated (and un-deprecated);
- an identical one is left alone, so applying the same file twice changes
  nothing;
- a metric of that CLI missing from the file gets `deprecated_at`.

`updated_by` records `--actor`, by default the OS user.

## Syncing from CI

Give the CLI's CI a key that can only sync catalogs:

```sh
argvio apikey create --tenant-id <TENANT_ID> --scope metrics_admin --dsn "$DSN"
```

and send the file on release:

```sh
argvio metric validate -f argvio.metrics.yaml   # optional, offline
curl -fsS -X PUT "$ARGVIO_METRICS_URL/v1/metric-definitions/vulnapi" \
  -H "Authorization: Bearer $ARGVIO_METRICS_ADMIN_KEY" \
  --data-binary @argvio.metrics.yaml
```

`PUT /v1/metric-definitions/{service}` does exactly what `argvio metric
apply` does and returns what changed (`created`, `updated`, `unchanged`,
`deprecated`). The file's `service` must be the one in the path. It
accepts a `metrics_admin` key or a user whose role in the tenant is `admin`;
a `metrics_query` key or a `viewer` gets `403 insufficient_scope`, and a
`metrics_admin` key gets the same on every read endpoint. An invalid file
is `400 invalid_catalog` with every problem listed. The body is at most
1 MiB. `updated_by` records the key's prefix or the user's issuer and
subject. The endpoint isn't open to browsers (CORS allows PUT only on the dashboard layout): it is
meant for CI.

## What ingest does with it

The public server checks every `cli.counter` and `cli.measure` data point
against the sending CLI's catalog (matched on `service.name` and the
metric name), after allowlist and consent-tier filtering and before its
dimensions are folded:

- **Declared metric:** dimensions not in its `dims` are dropped (and logged
  by key), a plain label too unless `dims` lists `label`; a point sent as
  another kind (a counter declared as a duration) is rejected; a point
  below its `min_tier` is rejected.
- **Undeclared metric** (or a deprecated one): accepted unchecked, so
  instrumenting needs no setup, and listed by `argvio metric list` and the
  API as undeclared. With the tenant's `strict_metrics` on (`argvio tenant
  config --strict-metrics`), it is rejected instead.

Rejections show in the OTLP response's `PartialSuccess` counts. Ingest
caches each tenant's catalog for `public.metric_catalog_cache_ttl` (60s), so
a catalog change applies within it.

## Dashboard layout

The Product view shows each CLI's metrics as cards. Which cards, in which
order and type, is the one thing the dashboard owns: `GET
/v1/dashboard/cards?service=` returns the saved layout, or a default built
from the catalog (a KPI plus a trend, or a distribution for values, per
declared metric). A tenant admin saves one with `PUT
/v1/dashboard/cards/{service}` (`{"cards": [{"metric", "card_type",
"options"}]}`, replaced whole; an empty list goes back to the default).
Card types are `kpi`, `trend`, `breakdown`, `distribution` (durations and
values) and `reach` (not ratios). A card can only be saved on a declared,
current metric; a card whose metric later leaves the file is served with
`status: deprecated` (or `missing`), not dropped.

## API

`GET /v1/metric-definitions` (`service=` repeatable) returns the tenant's
definitions, each with `service`, `name`, `kind`, `definition` (the entry
as the file states it, defaults filled in), `deprecated_at`, `updated_at`
and `updated_by`. It also lists the counters and measures the tenant's CLIs
sent over the last 30 days without declaring them, with `declared: false`
and no `definition`, so a dashboard can show them and suggest declaring them.
A declared and a sent metric match on service, name and kind. The endpoint
takes no time range. See `openapi/openapi.yaml`.

Definitions are stored in `metric_definitions` (docs/schema.md).
