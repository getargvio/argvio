# Sending data to Argvio and another backend via the OTel Collector

Argvio only accepts CLI telemetry that satisfies the allowlist/consent-tier
schema (docs/allowlist.md) — it isn't a general-purpose telemetry sink. If
you also want the same data in a general-purpose backend (Honeycomb,
Datadog, Grafana Cloud, a self-hosted Jaeger/Prometheus/Loki stack, etc.),
run an OpenTelemetry Collector in front of both: one set of receivers, one
pipeline per signal, two exporters per pipeline. The collector fans out —
it does not choose one destination over the other.

This only covers **traces and logs**, since that's what `cli.analytics.tier`
gated ingest supports today (`schema/allowlist/v1.yaml`'s
`supported_signal_types`); add a `metrics` pipeline the same way if/when
metrics are enabled for your tenant.

## Prerequisites

- A tenant and a `public_ingest`-scoped API key (docs/running-locally.md,
  "Seed a tenant + API key"). Argvio has no self-serve onboarding — the raw
  key is issued once by `argvio apikey create` and only its hash is
  stored.
- Every resource span/log record must carry `cli.analytics.tier`
  (docs/allowlist.md) and use only allowlisted event names/attributes.
  Records missing the tier attribute, or naming an event Argvio doesn't
  recognize, are rejected outright — this applies regardless of what other
  backend you're also sending to.

## Collector config

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

exporters:
  # Argvio's OTLP/gRPC receiver.
  otlp/argvio:
    endpoint: ingest.example.com:4317   # your public.grpc_listen_addr, TLS-fronted in prod
    headers:
      x-argvio-api-key: ${env:ARGVIO_API_KEY}
    tls:
      insecure: false   # set true only against a local dev server without TLS

  # Any second backend, OTLP or otherwise. Example: another OTLP/HTTP collector/vendor.
  otlphttp/other:
    endpoint: https://otel-collector.other-vendor.example.com
    headers:
      Authorization: Bearer ${env:OTHER_VENDOR_API_KEY}

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: []
      exporters: [otlp/argvio, otlphttp/other]

    logs:
      receivers: [otlp]
      processors: []
      exporters: [otlp/argvio, otlphttp/other]
```

Listing two exporters under the same pipeline's `exporters:` is the whole
mechanism — the collector duplicates each batch and sends it to both
independently. A slow or erroring exporter doesn't block the other (each
exporter has its own queue/retry settings; see the `sending_queue`/`retry_on_failure`
options in the [OTel Collector exporter helper
docs](https://github.com/open-telemetry/opentelemetry-collector/blob/main/exporter/exporterhelper/README.md)
if you want to tune those per destination).

Argvio also accepts the API key over a standard `Authorization: Bearer
<key>` header instead of `x-argvio-api-key`, for collector distributions or
vendor exporters that only let you set one auth header shape
(`internal/otlp/http.go`'s `httpAPIKey`, gRPC equivalent in
`internal/otlp/grpc.go`). Prefer `x-argvio-api-key` when you can, since it
won't collide with a routing processor or another exporter that also wants
to set `Authorization`.

## Splitting traffic instead of duplicating it

If instead you want *some* data to go only to Argvio and the rest only
elsewhere (e.g. CLI command telemetry to Argvio, everything else to your
general APM), use the [routing
connector](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/connector/routingconnector)
or two separate receivers/pipelines rather than the fanout above — sending
non-CLI-shaped data to Argvio's ingest path will just get it rejected by
the allowlist, so routing is more efficient than fanning out and letting
Argvio drop what it doesn't recognize.

## Verifying both destinations are receiving data

- **Argvio**: query it back through the `metrics` server, e.g.
  `GET /v1/traces?...` (docs/running-locally.md, "Query it back") or check
  `argvio`/Postgres directly.
- **Other backend**: use whatever that vendor/stack provides. If nothing
  arrives at one side, check the collector's own logs/telemetry first
  (`service.telemetry.logs.level: debug` surfaces per-exporter send
  failures, including Argvio's `401`/`403`/`429` responses — see
  `httpStatusForAuthError` in `internal/otlp/http.go` for what those codes
  mean on the Argvio side: `401` invalid/missing key, `403` tenant
  suspended, `429` rate-limited).

## Related docs

- [`docs/allowlist.md`](allowlist.md) — required attributes, consent
  tiers, and why an event might be rejected.
- [`docs/configuration.md`](configuration.md) — `public` server config
  (listen addresses, rate limits, batch/attribute limits enforced against
  whatever the collector sends).
- [`docs/running-locally.md`](running-locally.md) — seeding a tenant and
  API key, sending a manual test payload.
