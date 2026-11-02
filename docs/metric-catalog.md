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
catalog read-only.

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
