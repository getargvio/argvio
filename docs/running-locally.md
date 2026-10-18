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

## Option C: a config file instead of env vars

Every `argvio` subcommand that reads config accepts a repeatable
`--config`/`-c` flag pointing at one YAML document covering `storage:`,
`public:`, and `metrics:` sections together — see
[`argvio.example.yaml`](../argvio.example.yaml). Env vars still override
whatever the file sets, so it's safe to keep secrets like `storage.dsn` out
of the file and inject them via env in production:

```sh
go run ./cmd/argvio --config ./argvio.example.yaml migrate up
go run ./cmd/argvio --config ./argvio.example.yaml serve public
go run ./cmd/argvio --config ./argvio.example.yaml serve metrics
```

Passing `-c` more than once layers files in the order given (later files
override earlier ones): `argvio -c base.yaml -c prod.yaml serve public`.

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

`auth.methods` defaults to `[api_key, jwt]` (global — see
docs/configuration.md), so an `api_key`-scoped token works out of the box:

```sh
curl -H "Authorization: Bearer <METRICS_QUERY_KEY>" \
  "http://localhost:8080/v1/traces?from=2026-08-01T00:00:00Z&limit=10"
```

Or, since `jwt` is also enabled by default, sign a token whose `tenant_id`
claim matches `<TENANT_ID>`, HS256, using `ARGVIO_AUTH__JWT_SIGNING_KEY` as
the secret — see `internal/authn/authn.go` for exactly what's validated
(algorithm pinned to HS256; `tenant_id` claim required). The same
`internal/authn.Verifier` and `auth.methods` config also apply to `public`
now, so a signed JWT works against the OTLP ingest endpoint too, not just
`metrics` — see docs/architecture.md.

To sign users in through an OIDC provider instead, add `oidc` to
`auth.methods`, set `auth.oidc.*` (see `argvio.example.yaml`), and grant each
user access to a tenant. The user is identified by the `iss` and `sub` of their
verified token:

```sh
go run ./cmd/argvio tenant member add --tenant <TENANT_ID_OR_SLUG> \
  --issuer https://idp.example.com --sub <USER_SUB> --role viewer --dsn "$DSN"
go run ./cmd/argvio tenant member list --tenant <TENANT_ID_OR_SLUG> --dsn "$DSN"
```

`--issuer` must match the provider's discovery `issuer` exactly (trailing slash
included). A user in several tenants calls `GET /v1/tenants` and then sends
`X-Argvio-Tenant: <tenant id>` on every request.

### Operating as an admin locally

To see every tenant without a `tenant_members` row each, recognise yourself as
an admin (`metrics` server only; see docs/architecture.md). The simplest rule
is an allowlist of your own `sub`:

```sh
export ARGVIO_AUTH__ADMIN__SUBJECTS=<YOUR_SUB>          # comma-separated for several
```

Your `sub` is the `sub` claim of your access token, and the allowlist is
matched together with the issuer that verified it (`auth.admin.oidc.issuer_url`,
else `auth.oidc.issuer_url`), so the same `sub` at another IdP does not match.
To read it, decode the payload of a token your IdP issued for the API (the
middle segment of the JWT, base64url):

```sh
echo "$ACCESS_TOKEN" | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null | jq '{iss, sub, aud}'
```

An opaque token has no payload to read; use the IdP's own user view (for
Auth0, the user's `user_id`, e.g. `auth0|abc123`) or the provider's userinfo
or introspection endpoint. Restart `serve metrics` after changing the list;
removing a `sub` and restarting revokes that admin. An admin has the
`viewer` role in every tenant unless `auth.admin.role` says otherwise.
Instead of (or as well as) subjects, a group claim can recognise admins:
`ARGVIO_AUTH__ADMIN__CLAIM__NAME=groups` with `ARGVIO_AUTH__ADMIN__CLAIM__VALUE=argvio-admins`.

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
