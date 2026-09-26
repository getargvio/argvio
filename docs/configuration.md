# Configuration reference

Generated from `internal/config`'s structs (`go run ./tools/gendocs`) — key names, env var names, types, and defaults come directly from the code that parses them; only the descriptions are hand-authored. Regenerate after changing any config struct.

Layering: **defaults → YAML config file → environment variable overrides** (highest precedence wins). Env vars use prefix `ARGVIO_` and `__` as the nesting delimiter (plain `_` is legal inside a key name), e.g. `ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS`.

## `public` server (`argvio serve public`)

| Key | Env var | Type | Default | Description |
|---|---|---|---|---|
| `log_level` | `ARGVIO_LOG_LEVEL` | string | `info` | Log level: debug, info, warn, error. |
| `public.grpc_listen_addr` | `ARGVIO_PUBLIC__GRPC_LISTEN_ADDR` | string | `0.0.0.0:4317` | OTLP/gRPC listen address. |
| `public.http_listen_addr` | `ARGVIO_PUBLIC__HTTP_LISTEN_ADDR` | string | `0.0.0.0:4318` | OTLP/HTTP listen address. |
| `public.tls.enabled` | `ARGVIO_PUBLIC__TLS__ENABLED` | bool | `false` | Enable TLS on both listeners. |
| `public.tls.cert_file` | `ARGVIO_PUBLIC__TLS__CERT_FILE` | string | `` | PEM certificate path (required if tls.enabled). |
| `public.tls.key_file` | `ARGVIO_PUBLIC__TLS__KEY_FILE` | string | `` | PEM private key path (required if tls.enabled). |
| `public.max_batch_size` | `ARGVIO_PUBLIC__MAX_BATCH_SIZE` | int | `1000` | Max spans/log records/data points per OTLP export request. Over this, the whole request is rejected (not partial success). |
| `public.max_attribute_count` | `ARGVIO_PUBLIC__MAX_ATTRIBUTE_COUNT` | int | `64` | Max attributes per resource or per record (span/log record/data point). Over this, the record is rejected. |
| `public.max_attribute_key_length` | `ARGVIO_PUBLIC__MAX_ATTRIBUTE_KEY_LENGTH` | int | `128` | Max attribute key length in bytes. Longer keys are dropped (record kept). |
| `public.max_attribute_string_value_length` | `ARGVIO_PUBLIC__MAX_ATTRIBUTE_STRING_VALUE_LENGTH` | int | `4096` | Max string attribute value length in bytes. Longer values are dropped (record kept). |
| `public.max_timestamp_skew_past` | `ARGVIO_PUBLIC__MAX_TIMESTAMP_SKEW_PAST` | duration | `24h` | Reject a record whose timestamp is older than now minus this. |
| `public.max_timestamp_skew_future` | `ARGVIO_PUBLIC__MAX_TIMESTAMP_SKEW_FUTURE` | duration | `5m` | Reject a record whose timestamp is later than now plus this. |
| `public.api_key_cache_ttl` | `ARGVIO_PUBLIC__API_KEY_CACHE_TTL` | duration | `60s` | TTL for the in-memory API-key resolution cache (positive and negative results). |
| `public.rate_limit_requests_per_second` | `ARGVIO_PUBLIC__RATE_LIMIT_REQUESTS_PER_SECOND` | float | `200` | Default per-tenant request rate limit (token bucket). Overridable per tenant in tenant_config. |
| `public.rate_limit_bytes_per_second` | `ARGVIO_PUBLIC__RATE_LIMIT_BYTES_PER_SECOND` | float | `5000000` | Default per-tenant byte-rate limit. Overridable per tenant. |
| `public.rate_limit_burst` | `ARGVIO_PUBLIC__RATE_LIMIT_BURST` | int | `400` | Default per-tenant request-burst size. Overridable per tenant. |
| `public.allowlist_schema_path` | `ARGVIO_PUBLIC__ALLOWLIST_SCHEMA_PATH` | string | `schema/allowlist/v1.yaml` | Path to the versioned allowlist/tier schema YAML (see docs/allowlist.md). |
| `public.allowlist_hot_reload` | `ARGVIO_PUBLIC__ALLOWLIST_HOT_RELOAD` | bool | `true` | Watch allowlist_schema_path and hot-reload on change without a restart. |
| `public.shutdown_grace_period` | `ARGVIO_PUBLIC__SHUTDOWN_GRACE_PERIOD` | duration | `15s` | Graceful-shutdown timeout for the HTTP listener on SIGTERM/SIGINT (gRPC uses GracefulStop with no separate timeout). |
| `storage.dsn` | `ARGVIO_STORAGE__DSN` | string | `` | Postgres/Timescale connection string (postgres://...). No default — must be set. |
| `storage.public_pool.max_conns` | `ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS` | int | `50` | Max connections in the public server's pgx pool. |
| `storage.public_pool.min_conns` | `ARGVIO_STORAGE__PUBLIC_POOL__MIN_CONNS` | int | `5` | Min (kept-warm) connections in the public server's pgx pool. |
| `storage.metrics_pool.max_conns` | `ARGVIO_STORAGE__METRICS_POOL__MAX_CONNS` | int | `20` | Max connections in the metrics server's pgx pool. Independent from public_pool so a slow analytical query can never starve ingest. |
| `storage.metrics_pool.min_conns` | `ARGVIO_STORAGE__METRICS_POOL__MIN_CONNS` | int | `2` | Min (kept-warm) connections in the metrics server's pgx pool. |
| `storage.statement_timeout` | `ARGVIO_STORAGE__STATEMENT_TIMEOUT` | duration | `30s` | Postgres statement_timeout applied to the metrics server's connections. |
| `storage.traces.chunk_interval` | `ARGVIO_STORAGE__TRACES__CHUNK_INTERVAL` | duration | `24h` | Timescale hypertable chunk_time_interval for traces, applied at hypertable-creation time only (migrations/0002). |
| `storage.traces.compression_after` | `ARGVIO_STORAGE__TRACES__COMPRESSION_AFTER` | duration | `168h` | Global compression policy age threshold for traces (see docs/schema.md). |
| `storage.traces.retention_after` | `ARGVIO_STORAGE__TRACES__RETENTION_AFTER` | duration | `720h` | Global retention (chunk-drop) policy age threshold for traces. |
| `storage.logs.chunk_interval` | `ARGVIO_STORAGE__LOGS__CHUNK_INTERVAL` | duration | `24h` | Timescale hypertable chunk_time_interval for logs. |
| `storage.logs.compression_after` | `ARGVIO_STORAGE__LOGS__COMPRESSION_AFTER` | duration | `168h` | Global compression policy age threshold for logs. |
| `storage.logs.retention_after` | `ARGVIO_STORAGE__LOGS__RETENTION_AFTER` | duration | `2160h` | Global retention policy age threshold for logs. |
| `storage.metrics.chunk_interval` | `ARGVIO_STORAGE__METRICS__CHUNK_INTERVAL` | duration | `24h` | Timescale hypertable chunk_time_interval for metrics. |
| `storage.metrics.compression_after` | `ARGVIO_STORAGE__METRICS__COMPRESSION_AFTER` | duration | `720h` | Global compression policy age threshold for metrics. |
| `storage.metrics.retention_after` | `ARGVIO_STORAGE__METRICS__RETENTION_AFTER` | duration | `9600h` | Global retention policy age threshold for metrics. |

## `metrics` server (`argvio serve metrics`)

| Key | Env var | Type | Default | Description |
|---|---|---|---|---|
| `log_level` | `ARGVIO_LOG_LEVEL` | string | `info` | Log level: debug, info, warn, error. |
| `metrics.listen_addr` | `ARGVIO_METRICS__LISTEN_ADDR` | string | `0.0.0.0:8080` | Metrics REST API listen address. |
| `metrics.tls.enabled` | `ARGVIO_METRICS__TLS__ENABLED` | bool | `false` | Enable TLS on the listener. |
| `metrics.tls.cert_file` | `ARGVIO_METRICS__TLS__CERT_FILE` | string | `` | PEM certificate path (required if tls.enabled). |
| `metrics.tls.key_file` | `ARGVIO_METRICS__TLS__KEY_FILE` | string | `` | PEM private key path (required if tls.enabled). |
| `metrics.auth_mode` | `ARGVIO_METRICS__AUTH_MODE` | string | `jwt` | jwt (dashboard-user session, HS256, requires jwt_signing_key) or api_key (scoped API key, scope=metrics_query). |
| `metrics.jwt_signing_key` | `ARGVIO_METRICS__JWT_SIGNING_KEY` | string | `` | HMAC secret for verifying dashboard JWTs. Required when auth_mode=jwt. |
| `metrics.query_timeout` | `ARGVIO_METRICS__QUERY_TIMEOUT` | duration | `10s` | Per-query server-side timeout. |
| `metrics.max_result_page_size` | `ARGVIO_METRICS__MAX_RESULT_PAGE_SIZE` | int | `1000` | Hard ceiling on ?limit=; requests above this are clamped. |
| `metrics.default_result_page_size` | `ARGVIO_METRICS__DEFAULT_RESULT_PAGE_SIZE` | int | `100` | ?limit= value used when the caller omits it. |
| `metrics.max_time_range_span` | `ARGVIO_METRICS__MAX_TIME_RANGE_SPAN` | duration | `2160h` | Reject a query whose to-from exceeds this (guards against an unbounded raw scan). |
| `metrics.dev_mode` | `ARGVIO_METRICS__DEV_MODE` | bool | `false` | Serve /openapi.yaml and /docs (Scalar viewer), unauthenticated. Must default false; never enable in production without an explicit decision. |
| `metrics.shutdown_grace_period` | `ARGVIO_METRICS__SHUTDOWN_GRACE_PERIOD` | duration | `15s` | Graceful-shutdown timeout for the HTTP listener on SIGTERM/SIGINT. |
| `storage.dsn` | `ARGVIO_STORAGE__DSN` | string | `` | Postgres/Timescale connection string (postgres://...). No default — must be set. |
| `storage.public_pool.max_conns` | `ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS` | int | `50` | Max connections in the public server's pgx pool. |
| `storage.public_pool.min_conns` | `ARGVIO_STORAGE__PUBLIC_POOL__MIN_CONNS` | int | `5` | Min (kept-warm) connections in the public server's pgx pool. |
| `storage.metrics_pool.max_conns` | `ARGVIO_STORAGE__METRICS_POOL__MAX_CONNS` | int | `20` | Max connections in the metrics server's pgx pool. Independent from public_pool so a slow analytical query can never starve ingest. |
| `storage.metrics_pool.min_conns` | `ARGVIO_STORAGE__METRICS_POOL__MIN_CONNS` | int | `2` | Min (kept-warm) connections in the metrics server's pgx pool. |
| `storage.statement_timeout` | `ARGVIO_STORAGE__STATEMENT_TIMEOUT` | duration | `30s` | Postgres statement_timeout applied to the metrics server's connections. |
| `storage.traces.chunk_interval` | `ARGVIO_STORAGE__TRACES__CHUNK_INTERVAL` | duration | `24h` | Timescale hypertable chunk_time_interval for traces, applied at hypertable-creation time only (migrations/0002). |
| `storage.traces.compression_after` | `ARGVIO_STORAGE__TRACES__COMPRESSION_AFTER` | duration | `168h` | Global compression policy age threshold for traces (see docs/schema.md). |
| `storage.traces.retention_after` | `ARGVIO_STORAGE__TRACES__RETENTION_AFTER` | duration | `720h` | Global retention (chunk-drop) policy age threshold for traces. |
| `storage.logs.chunk_interval` | `ARGVIO_STORAGE__LOGS__CHUNK_INTERVAL` | duration | `24h` | Timescale hypertable chunk_time_interval for logs. |
| `storage.logs.compression_after` | `ARGVIO_STORAGE__LOGS__COMPRESSION_AFTER` | duration | `168h` | Global compression policy age threshold for logs. |
| `storage.logs.retention_after` | `ARGVIO_STORAGE__LOGS__RETENTION_AFTER` | duration | `2160h` | Global retention policy age threshold for logs. |
| `storage.metrics.chunk_interval` | `ARGVIO_STORAGE__METRICS__CHUNK_INTERVAL` | duration | `24h` | Timescale hypertable chunk_time_interval for metrics. |
| `storage.metrics.compression_after` | `ARGVIO_STORAGE__METRICS__COMPRESSION_AFTER` | duration | `720h` | Global compression policy age threshold for metrics. |
| `storage.metrics.retention_after` | `ARGVIO_STORAGE__METRICS__RETENTION_AFTER` | duration | `9600h` | Global retention policy age threshold for metrics. |

## Per-tenant overrides

Stored in Postgres (`tenant_config` table), not static config — they change per-customer at runtime via `argvio tenant config`. See docs/schema.md.

| Column | Meaning |
|---|---|
| `tier_ceiling` | Max consent tier this tenant may send (anonymous/basic/full/optin_plus). |
| `tier_enforcement_mode` | `strip` or `reject` when a record's declared tier exceeds the ceiling. |
| `rate_limit_requests_per_sec` / `rate_limit_bytes_per_sec` / `rate_limit_burst` | Override the public server's global rate-limit defaults. NULL = use global default. |
| `retention_traces_days` / `retention_logs_days` / `retention_metrics_days` | Shorter-than-global retention, enforced by `argvio retention sweep` (see docs/schema.md). NULL = use the global Timescale retention policy. |
