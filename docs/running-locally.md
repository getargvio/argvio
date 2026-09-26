# Running locally

## Prerequisites

- Go 1.26+
- Docker (for Postgres+TimescaleDB — see docs/schema.md's "Deployment
  prerequisite" for why the image matters: it must include
  `timescaledb_toolkit`, not just `timescaledb`)

## Option A: docker compose (everything)

```sh
docker compose up --build
```

This builds one image (`.docker/Dockerfile-build`, the single `argvio`
binary) and runs four containers, each running `argvio` with a different
command: `postgres` (`timescale/timescaledb-ha:pg16`), a one-shot
`migrate` job (`argvio migrate up`, waits for Postgres's healthcheck), then
`public` (`argvio serve public`, ports 4317 gRPC / 4318 HTTP) and `metrics`
(`argvio serve metrics`, port 8080, started with `dev_mode=true` so
`/openapi.yaml` and `/docs` are served).

Skip to "Seed a tenant" once all four containers are up
(`docker compose ps`).

## Option B: Postgres in Docker, servers via `go run`

```sh
docker run -d --name argvio-pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=argvio \
  -p 5432:5432 timescale/timescaledb-ha:pg16

export ARGVIO_STORAGE__DSN="postgres://postgres:postgres@localhost:5432/argvio?sslmode=disable"

go run ./cmd/argvio migrate up

# terminal 2
export ARGVIO_STORAGE__DSN="postgres://postgres:postgres@localhost:5432/argvio?sslmode=disable"
go run ./cmd/argvio serve public

# terminal 3
export ARGVIO_STORAGE__DSN="postgres://postgres:postgres@localhost:5432/argvio?sslmode=disable"
export ARGVIO_METRICS__JWT_SIGNING_KEY="dev-only-signing-key-change-me"
export ARGVIO_METRICS__DEV_MODE=true
go run ./cmd/argvio serve metrics
```

Full config reference (every key, env var, default): docs/configuration.md.

## Seed a tenant + API key

```sh
export DSN="postgres://postgres:postgres@localhost:5432/argvio?sslmode=disable"
# (docker compose: run these as `docker compose run --rm migrate /app/argvio ...` instead)

go run ./cmd/argvio tenant create --slug acme --name "Acme Inc" --dsn "$DSN"
# -> created tenant acme (id=<TENANT_ID>) ...

go run ./cmd/argvio tenant config --tenant-id <TENANT_ID> --tier-ceiling full --dsn "$DSN"

go run ./cmd/argvio apikey create --tenant-id <TENANT_ID> --scope public_ingest --dsn "$DSN"
# -> RAW KEY (shown once, store it now): argv_live_pub_...
```

The raw key is only ever shown at creation — only its SHA-256 hash is
stored (`api_keys.key_hash`).

## Send a test OTLP payload

Any OTel SDK/collector pointed at `localhost:4317` (gRPC) or
`localhost:4318` (HTTP) with header `x-argvio-api-key: <RAW_KEY>` works.
Every resource **must** carry `cli.analytics.tier` (see docs/allowlist.md)
— a request missing it is rejected. A minimal Go client:

```go
conn, _ := grpc.NewClient("localhost:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
client := ptraceotlp.NewGRPCClient(conn)

td := ptrace.NewTraces()
rs := td.ResourceSpans().AppendEmpty()
rs.Resource().Attributes().PutStr("cli.analytics.tier", "anonymous")
span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
span.SetName("cli.command.deploy")               // must match an allowed span pattern
span.Attributes().PutStr("cli.command.name", "deploy")
span.SetStartTimestamp(pcommon.NewTimestampFromTime(time.Now()))
span.SetEndTimestamp(pcommon.NewTimestampFromTime(time.Now().Add(100 * time.Millisecond)))

ctx := metadata.AppendToOutgoingContext(context.Background(), "x-argvio-api-key", rawKey)
resp, _ := client.Export(ctx, ptraceotlp.NewExportRequestFromTraces(td))
fmt.Println("rejected_spans:", resp.PartialSuccess().RejectedSpans())
```

## Query it back

```sh
# metrics_query-scoped API key (simpler for a curl smoke test than minting a JWT):
go run ./cmd/argvio apikey create --tenant-id <TENANT_ID> --scope metrics_query --dsn "$DSN"
```

With `auth_mode=api_key` (`ARGVIO_METRICS__AUTH_MODE=api_key` — default is
`jwt`, which needs a signed token instead, see docs/configuration.md):

```sh
curl -H "Authorization: Bearer <METRICS_QUERY_KEY>" \
  "http://localhost:8080/v1/traces?from=2026-08-01T00:00:00Z&limit=10"
```

With the default `jwt` auth mode, sign a token whose `tenant_id` claim
matches `<TENANT_ID>`, HS256, using `ARGVIO_METRICS__JWT_SIGNING_KEY` as
the secret — see `internal/metricsapi/auth.go` for exactly what's
validated (algorithm pinned to HS256; `tenant_id` claim required).

## Continuous aggregates in local dev

Continuous aggregates refresh on a schedule (`add_continuous_aggregate_policy`,
hourly — see docs/schema.md). For quick local testing, without waiting:

```sql
CALL refresh_continuous_aggregate('cagg_command_stats_hourly', NULL, NULL);
CALL refresh_continuous_aggregate('cagg_command_stats_daily', NULL, NULL);
CALL refresh_continuous_aggregate('cagg_exit_codes_hourly', NULL, NULL);
CALL refresh_continuous_aggregate('cagg_session_activity_daily', NULL, NULL);
```

Every `/v1/metrics/*` aggregate endpoint except `/v1/traces` reads from one
of these four views (see docs/schema.md) — they'll return empty until at
least one refresh has run. `/v1/metrics/retention` is the one exception:
it reads raw `logs` directly, so it needs no refresh, only ingested data.

## API docs (dev only)

With `ARGVIO_METRICS__DEV_MODE=true`: `http://localhost:8080/docs` (Scalar
viewer) and `http://localhost:8080/openapi.yaml` (raw spec). Never enable
`dev_mode` in production without deciding to explicitly — see
docs/configuration.md.

## Running the test suite

Most packages have plain unit tests (`go test ./...`). The packages that
touch Postgres (`internal/storage`, `internal/tenant`, `internal/otlp`,
`internal/metricsapi`) have integration tests gated behind
`ARGVIO_TEST_DSN` — they run for real against a live Timescale instance
(migrating, seeding, and tearing down their own data) rather than mocking
the database:

```sh
export ARGVIO_TEST_DSN="postgres://postgres:postgres@localhost:5432/argvio?sslmode=disable"
go test ./...
```

Without `ARGVIO_TEST_DSN` set, those tests skip (not fail).
