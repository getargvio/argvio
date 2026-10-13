# Health checks

Modeled on ory/hydra, which exposes plain HTTP liveness/readiness endpoints
and leaves probing to the orchestrator (its own quickstart compose files only
define a `healthcheck` for Postgres, not for Hydra itself).

## What each server exposes

| Server | Endpoint | Auth | Meaning |
| --- | --- | --- | --- |
| `argvio serve metrics` | `GET /healthz` on `metrics.listen_addr` (default `:8080`) | none | Process is up and serving HTTP. Returns `200 ok`. |
| `argvio serve public` | `GET /healthz` on `public.http_listen_addr` (default `:4318`) | none | Same, on the OTLP/HTTP listener. |

`/healthz` is liveness only: it does not check the database. Hydra splits
this into `/health/alive` and `/health/ready`; argvio has no readiness
endpoint yet.

```sh
curl -fsS http://localhost:8080/healthz   # -> ok
```

## The image has no shell or curl

The release and build images are `gcr.io/distroless/static-debian13:nonroot`,
so a `healthcheck` using `curl` or `CMD-SHELL` fails. The binary probes
itself instead:

```sh
argvio healthcheck metrics   # GET /healthz on metrics.listen_addr
argvio healthcheck public    # GET /healthz on public.http_listen_addr
```

Exit code 0 means HTTP 200 within 3s; anything else exits non-zero. It reads
the same config as `serve` (`-c`, `ARGVIO_*` env), so pass the same
`listen_addr` settings to the probe. Wildcard binds (`0.0.0.0`, `::`) are
probed via `127.0.0.1`. TLS is not yet wired into the servers, so the probe
uses plain HTTP.

### Docker Compose

```yaml
healthcheck:
  test: ["CMD", "/app/argvio", "healthcheck", "metrics"]
  interval: 10s
  timeout: 5s
  retries: 3
  start_period: 5s
```

See `docker-compose.yml` for the `public` and `metrics` services.

### Kubernetes

`httpGet` probes run in the kubelet, so they work without the subcommand:

```yaml
livenessProbe:
  httpGet: { path: /healthz, port: 8080 }   # 4318 for `serve public`
  periodSeconds: 10
  failureThreshold: 3
```

Or use `exec: { command: ["/app/argvio", "healthcheck", "metrics"] }`.

## Not done yet

- a `/readyz` that pings the database.
