# Product metrics: per-project, vendor-defined metrics

Status: proposal, nothing implemented. Written after reading
`argvio-go/counter.go`, `argvio-go/measure.go`, `internal/storage/counters.go`,
migrations 0013–0014, the `/v1/metrics/counters` and `/v1/metrics/measures`
endpoints, the UI's `CountersSection`/`MeasuresSection`, and the OTel
instrumentation vulnapi ships today (`scan/scan.go`, `openapi/operation.go`,
`openapi/security_scheme.go`).

"Project" below means one CLI. A tenant may ship several CLIs, told apart by
the OTel resource attribute `service.name`, which both SDKs already set to
the CLI's name. Every product metric, definition and dashboard is therefore
scoped by **(tenant, service)**.

## Goal

Every CLI has a few numbers that say whether it delivers value, and none of
them are generic: vulnapi finds **vulnerabilities**, a linter reports
**violations fixed**, a deploy tool counts **resources changed**, a code
generator emits **files generated**. Argvio should let each project define,
send, and read *its own* metrics as first-class citizens, with the same
filters (version, OS, CI, compare) as the built-in analytics. That should
need no server change and no dashboard code per project.

## What already exists

| Piece | What it gives | Where |
| --- | --- | --- |
| `cli.counter` | delta sum, `name` + **one** `label`, accepted at `anonymous` | `RecordCounter` in both SDKs |
| `cli.measure` | histogram, **milliseconds only**, fixed ms buckets, `name` + one `label` | `RecordDuration` |
| Vocabulary caps | per-tenant caps on names and labels, overflow folded to `other` | `counter_names`/`counter_labels`/`measure_*` |
| Rollups | `cagg_counters_{hourly,daily}`, `cagg_measures_{hourly,daily}` | migrations 0013, 0014 |
| API | list + per-name series and label breakdown | `/v1/metrics/counters`, `/v1/metrics/measures` |
| UI | one generic table + chart each, at the bottom of the Usage view | `CountersSection`, `MeasuresSection` |

That is a solid base: transport, cardinality control, retention and rollups
are done. What's missing is everything that turns a raw counter into a
*product metric*.

## Worked example: what vulnapi wants to know

| Question | Can it be answered today? |
| --- | --- |
| Total vulnerabilities found this week | Yes: `RecordCounter("vulns.found", n)` |
| ...by severity | Yes, with `WithLabel("high")` |
| ...by severity **and** by check (`bola`, `jwt.alg_none`, ...) | **No**, one label only. Using `"high.bola"` as the label breaks the label cap and any per-severity rollup |
| Vulnerabilities **per scan** (median, p90) | **No**, a sum loses the per-run shape. `cli.measure` is ms-only |
| Share of scans that found at least one vulnerability | **No** |
| Vulnerabilities per 100 operations scanned | **No**, no ratio between two metrics |
| Share of active installs that found something this month | **No**, counters carry no install id |
| Did detection drop after release 0.9.0? | Partly: filter by version, but no per-scan normalisation, so more usage looks like more detection |
| A readable dashboard ("Vulnerabilities found", unit, description) instead of `vulns.found` at the bottom of Usage | **No** |

vulnapi's current instrumentation shows the same needs: `scan.operation_scan_handler.counter`
is split by check `id`, `openapi.operation.counter` carries several attributes
at once (method, security scheme types), and it records counts that are
really per-run sizes (headers and cookies per operation). It uses raw OTel
instruments with free-form attributes, which Argvio's allowlist doesn't
accept as they are. The features below give it a supported path.

## F0. Scope vendor metrics by CLI

Today `cagg_counters_*` and `cagg_measures_*` group by tenant only, so two CLIs
sending `scan.count` are summed together. The rollups gain a `service_name`
column (`resource_attributes ->> 'service.name'`), rebuilt from the raw
`metrics` hypertable, whose 400-day retention covers everything sent since
migration 0013. The counters and measures endpoints take a `service` filter, and
`GET /v1/metrics/services` lists the CLIs a tenant has sent vendor metrics for.
Vocabulary caps stay per tenant: they protect the tenant's storage, whichever
CLI fills it.

## Features

Six features on top of F0, in the order they're needed. F1–F3 are the core;
F4–F6 build on them.

### F1. Multi-dimension metrics

Allow up to **3 named dimensions** per data point instead of one anonymous
label.

- **Wire.** Dimensions are sent as `cli.dim.<key>` attributes
  (`cli.dim.severity=high`, `cli.dim.check=bola`). Keys and values follow
  the existing identifier rule `^[a-z0-9][a-z0-9._-]*$`, ≤ 64 bytes. More
  than 3 dims means the data point is dropped by the SDK and rejected by
  ingest, never truncated, so a series can't silently merge into another.
  The existing `cli.counter.label` / `cli.measure.label` is read as the
  dimension `label`, so current SDKs keep working unchanged.
- **Caps are configuration, not constants.** Two new keys,
  `public.max_metric_dims` (default 3) and `public.max_dim_values_per_key`
  (default 50), join the existing ones. A dimension tuple *is* the stored
  label, so the existing `max_counter_labels_per_name` /
  `max_measure_labels_per_name` cap the tuples per name; there is no
  second key for the same thing. Every vendor-metric cap can also be
  overridden per tenant in `tenant_config` (`argvio tenant config set`), so a
  project with 300 checks can be given room without raising everyone's limit.
- **Ingest.** Canonicalise the dims into one sorted string,
  `check=bola;severity=high`, stored in the attribute the rollups already
  group by. **The cagg schema doesn't change for dims**: the existing `label`
  column holds the canonical tuple, and a legacy label without `=` reads as
  `label=<value>`. Admission (`Writer.admit`) keeps capping tuples per name
  and additionally caps each **dim key's values** per name. Overflow folds
  the offending value to `other` *inside the tuple*:
  `check=other;severity=high` keeps the severity breakdown intact.
- **Query.** `GET /v1/metrics/counters?name=vulns.found&group_by=severity`
  sums tuples per value of one dim. `&where=check:bola` filters on another.
  The tuple cap keeps this bounded, so it runs in Go over the rows the
  rollup returns, like measure percentiles already do. Two-dim `group_by` is
  allowed for tables (heatmap: severity × check).

### F2. Value distributions (`cli.value`)

A histogram for **non-time quantities**, recorded once per run: findings
per scan, operations scanned, spec size, files generated.

- **Wire.** New metric `cli.value`, delta histogram, with `cli.value.name`,
  F1 dims, and `cli.value.unit` from a closed enum (`1`, `By`, `%`). Fixed
  bucket bounds shared across SDKs, like `measureBucketsMs`. They're
  zero-aware and roughly log-spaced: `0, 1, 2, 5, 10, 20, 50, 100, 200,
  500, 1000, 5000, 10000, 100000`. The **explicit `0` bucket** matters: it is
  what gives "share of scans that found nothing".
- **Storage.** `cagg_values_{hourly,daily}` mirroring `cagg_measures_*`:
  count, sum, min, max, one column per bucket, and percentiles interpolated
  in Go. Vocabulary tables `value_names`/`value_tuples` with their own caps.
- **Query.** `/v1/metrics/values`: p50/p90/p99, mean, and **hit rate**
  (`1 − bucket₀ / count`), per bucket of time and per dim.
- **Why not reuse `cli.measure` with a unit?** Its buckets are ms, merged
  across SDKs by contract. Changing them per name would break that
  contract and the cagg bucket columns. A sibling metric costs one
  migration and keeps both simple.

Gauges ("current value") are deliberately left out: a CLI is short-lived,
so "last value per run" *is* a distribution.

### F3. Metric catalog (per-project definitions)

A table of what each metric **means**, owned by the project. **The YAML file
in the CLI's repository is the only source of truth**: nothing edits a
definition except applying that file, and the UI displays definitions
read-only.

```yaml
# argvio.metrics.yaml — lives in the CLI's repo, synced by CI
version: 1
service: vulnapi            # must match the SDK's CLI name (service.name)
metrics:
  - name: vulns.found
    kind: counter            # counter | value | duration | ratio
    title: Vulnerabilities found
    description: Findings reported by a scan, after deduplication.
    unit: "1"
    dims: [severity, check]
    direction: up_is_good    # up_is_good | down_is_good | neutral
    dim_order:
      severity: [critical, high, medium, low, info]
  - name: scan.findings
    kind: value
    title: Findings per scan
    unit: "1"
    dims: [severity]
  - name: scan.operations
    kind: value
    title: Operations scanned per run
```

- **Storage.** `metric_definitions` (tenant_id, service_name, name, kind, title,
  description, unit, dims, direction, dim_order jsonb, min_tier,
  deprecated_at, updated_by, updated_at).
- **Write paths, all of them "apply the file".** `argvio metric apply -f
  argvio.metrics.yaml --tenant …` (operator CLI) and `PUT
  /v1/metric-definitions/{service}` with the file as the body. Applying
  *replaces* the service's whole catalog: a metric removed from the file is
  marked `deprecated_at`, not kept as a stale override. The endpoint requires
  the `admin` role, the first real use of the role `architecture.md` reserves
  for write endpoints, or an API key with the new `scope=metrics_admin`, so a
  vendor's CI syncs the file on release. There is no per-field edit endpoint.
- **Ingest uses it, lightly.** Definitions are cached like tenant config.
  A declared metric with undeclared dim keys gets them dropped (and logged
  as a per-attribute rejection). A kind mismatch rejects the data point.
  **Undeclared metrics are still accepted** and show as "undeclared" in the
  UI, so instrumenting stays zero-setup. An opt-in tenant flag
  `strict_metrics` rejects undeclared names, which is better than folding
  into `other` once a project knows its vocabulary.
- **Read.** `GET /v1/metric-definitions` merges declared definitions with
  names discovered in `counter_names`/`value_names`/`measure_names`, so the
  UI renders titles, units, ordering and direction without per-project
  code.

### F4. Derived metrics (ratios)

A catalog entry of `kind: ratio`, computed **at query time** from existing
rollups (nothing stored):

```yaml
  - name: vulns.per_100_ops
    kind: ratio
    title: Vulnerabilities per 100 operations
    numerator:   { metric: vulns.found }
    denominator: { metric: scan.operations, agg: sum }
    scale: 100
  - name: vulns.critical_per_scan
    kind: ratio
    numerator:   { metric: vulns.found, where: { severity: critical } }
    denominator: { builtin: command_invocations, where: { command: scan } }
```

Denominators may be another metric, or a built-in series: command
invocations, sessions, or active installs. Both sides are evaluated with
the same filters and time buckets, then divided in Go. A zero denominator
returns `null`, never `0`. This answers "did detection drop after 0.9.0?"
without usage growth skewing it.

### F5. Reach: installs behind a metric

"How many (or what share of) active installs found ≥ 1 vulnerability."

- At `basic` and above, the SDK adds `cli.install.id` to `cli.counter` /
  `cli.value` data points. It's one value per process, so OTel's in-memory
  aggregation doesn't grow. At `anonymous` it is omitted, as today.
- The rollups add a `hyperloglog(install_id)` column. This is the same
  technique as `cagg_session_footprint_daily`. Query returns
  `installs_reached` plus `installs_reported`, the coverage at basic+.
  Reach is therefore an explicit **lower bound**, and the UI says so.
- Catalog `min_tier` lets a project keep a sensitive metric off the
  anonymous tier entirely.

### F6. Product metrics view

A new top-level **Product** page in the UI, generated from the catalog.
It moves counters and measures out of the bottom of Usage.

| Card | Built from | Shows |
| --- | --- | --- |
| KPI tile | counter / ratio | total over the window, Δ vs previous window, coloured by `direction` |
| Trend | any | series per bucket, optional `group_by` dim, compare-mode overlay |
| Breakdown | counter / value | bars per dim value, in `dim_order`; heatmap for two dims |
| Distribution | value / duration | histogram, p50/p90, hit rate |
| Reach | counter / value with F5 | installs reached, % of active installs, coverage note |

- With no configuration, every declared metric gets a KPI tile and a trend,
  so a project sees something the moment the catalog exists.
- Pinning, ordering and card type are stored per tenant and service
  (`dashboard_cards`: tenant_id, service_name, position, metric, card_type,
  options jsonb), written with the same `admin` role as F3. Layout is the
  one thing the UI owns; definitions never are.
- A CLI selector (from `GET /v1/metrics/services`) scopes the whole page.
- Global filters (version, OS, arch, CI, compare A/B) apply as on every
  other view. "Compare 0.8 vs 0.9 on vulns per 100 operations" is the
  release-regression check vulnapi needs.

## SDK surface

Typed handles declared once, so dims are checked when the handle is made,
not on every call. The existing `RecordCounter`/`RecordDuration` stay as
sugar for the no-dims case.

```go
vulns := client.Counter("vulns.found", argvio.Dims("severity", "check"))
perScan := client.Value("scan.findings", argvio.Unit("1"), argvio.Dims("severity"))

for _, f := range report.Findings {
	vulns.Add(ctx, 1, f.Severity, string(f.CheckID)) // positional, matches Dims
}
for sev, n := range report.CountBySeverity() {
	perScan.Record(ctx, float64(n), sev) // once per scan, zero included
}
```

```ts
const vulns = client.counter('vulns.found', { dims: ['severity', 'check'] });
vulns.add(1, { severity: 'high', check: 'bola' });
```

Rules, enforced the same way in both SDKs:

- A wrong dim count or an invalid value drops the call. It never panics or
  blocks, and a nil or disabled client is a no-op.
- **Record zeros for `value` metrics.** A scan with no findings must
  record `0`, or the hit rate is wrong. Each SDK documents this, and the
  handle offers `RecordZeroIfUnset` at flush for per-run values.
- Same warning as today: names, keys and values are sent at the anonymous
  tier and must be fixed identifiers from code. Severity and check id are
  fine; an endpoint path is not.

## Privacy and abuse

- Dims widen what an anonymous data point can carry, from 1 identifier to
  3. Mitigations: the identifier regex, per-key value caps, per-name tuple
  caps, catalog `min_tier`, and `strict_metrics`.
- Reach (F5) is basic+ only and only ever exposed as an HLL estimate, never
  a list of installs.
- Catalog writes are `admin`-role or `metrics_admin`-key only, and are
  audit-logged (`updated_by`).

## Phasing

| Phase | Ships | Unblocks for vulnapi |
| --- | --- | --- |
| 1 | F0 per-CLI scope, F1 multi-dim + per-tenant caps, F3 catalog (read + `argvio metric apply`), F6 view with KPI/trend/breakdown, typed SDK handles | vulns by severity × check, a real "Vulnerabilities found" page |
| 2 | F2 `cli.value`, F4 ratios, distribution cards | findings per scan, hit rate, per-100-ops normalisation, release comparison |
| 3 | F5 reach, `strict_metrics`, `PUT /v1/metric-definitions` + `metrics_admin` keys, pinned cards | % of installs getting value, catalog synced from vulnapi's CI |

Later, not designed here: **alerts** on product metrics (e.g. "vulns per
100 ops fell 50% after a release"), and release annotations on trend
charts.

## Decisions

1. A tenant may ship several CLIs: everything is scoped by `service.name`
   (F0).
2. `argvio.metrics.yaml` is the source of truth for definitions. The UI only
   owns card layout.
3. Every cap is configurable globally, and overridable per tenant.
