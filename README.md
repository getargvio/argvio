# Argvio data component

CLI telemetry ingest + analytics: an OTLP receiver and a query API sharing
a Postgres/TimescaleDB storage layer, run as two independent server
processes. Infrastructure only — no billing, no tenant-onboarding UI, no
frontend, no CLI-side SDK.

One binary, `argvio` (`cmd/argvio`), one entrypoint — the two servers and
the operator CLI are all subcommands of it:

- **`argvio serve public`** — spec-compliant OTLP receiver (gRPC + HTTP)
  for metrics, logs, traces. Internet-facing, high-volume, untrusted input.
- **`argvio serve metrics`** — read-oriented REST API: filtering +
  aggregation (percentile latencies, error rate, command frequency,
  exit-code distribution, CI-vs-interactive split, session cohorts, active
  installs, cohort retention) plus a distinct filter-values lookup, over
  ingested telemetry. Internal/trusted-tenant-facing.
- **`argvio migrate` / `tenant` / `apikey` / `policies` / `retention`** —
  operator CLI: migrations, tenant/API-key seeding, Timescale policy
  reconciliation, per-tenant retention sweeps.

## Docs

- [`docs/architecture.md`](docs/architecture.md) — the two-server split,
  why, data flow.
- [`docs/schema.md`](docs/schema.md) — the Postgres/Timescale schema,
  table by table.
- [`docs/allowlist.md`](docs/allowlist.md) — the supported-data allowlist
  and consent-tier schema, how to extend it.
- [`docs/configuration.md`](docs/configuration.md) — every config key,
  generated from the config structs.
- [`docs/running-locally.md`](docs/running-locally.md) — docker compose,
  migrations, seeding a tenant, sending a test OTLP payload.
- [`docs/otel-collector.md`](docs/otel-collector.md) — fan out an OTel
  Collector to Argvio and another backend at the same time.
- [`openapi/openapi.yaml`](openapi/openapi.yaml) — the `metrics` server's
  REST API, endpoint by endpoint.

## Quick start

```sh
docker compose up --build
```

Then see [`docs/running-locally.md`](docs/running-locally.md) to seed a
tenant and send a test payload.
