-- Tenant/API-key/config tables. Plain (non-hypertable) Postgres tables —
-- low volume, needs strong consistency, read on every ingest request (via
-- the internal/tenant cache, not per-request).
CREATE EXTENSION IF NOT EXISTS pgcrypto; -- gen_random_uuid()

CREATE TABLE tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per tenant, 1:1 with tenants. Split out from `tenants` so
-- runtime-tunable overrides (rate limits, tier ceiling, retention) live in
-- an obviously-mutable place, separate from tenant identity.
CREATE TABLE tenant_config (
    tenant_id                  UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,

    -- Consent tier enforcement (internal/consent). tier_ceiling must be one
    -- of the consent_tiers.name values in the active allowlist schema;
    -- enforced in application code (loaded schema), not a DB constraint,
    -- since the schema is versioned/hot-reloadable independent of the DB.
    tier_ceiling                TEXT NOT NULL DEFAULT 'basic',
    tier_enforcement_mode       TEXT NOT NULL DEFAULT 'strip' CHECK (tier_enforcement_mode IN ('strip', 'reject')),

    -- NULL = fall back to the public server's global config default.
    rate_limit_requests_per_sec DOUBLE PRECISION,
    rate_limit_bytes_per_sec    DOUBLE PRECISION,
    rate_limit_burst            INTEGER,

    -- NULL = fall back to the global Timescale retention policy (the
    -- longest window configured). A non-null value shorter than the global
    -- policy is enforced by the `cmd/admin retention sweep` job, since
    -- Timescale's chunk-drop retention policies operate per-hypertable, not
    -- per-tenant, once tenant_id space-partitioning is in play — see
    -- docs/schema.md "Per-tenant retention" for why.
    retention_traces_days       INTEGER,
    retention_logs_days         INTEGER,
    retention_metrics_days      INTEGER,

    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- SHA-256 of the raw key; the raw key is shown once at creation time
    -- (cmd/admin) and never stored or logged again.
    key_hash     BYTEA NOT NULL,
    -- First 8 chars of the raw key, stored in the clear for display/lookup
    -- in admin tooling ("key argv_live_a1b2c3d4...") without exposing the
    -- full secret.
    key_prefix   TEXT NOT NULL,
    -- public_ingest: usable against the `public` OTLP server.
    -- metrics_query: usable against the `metrics` REST API (AuthModeAPIKey).
    scope        TEXT NOT NULL CHECK (scope IN ('public_ingest', 'metrics_query')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_api_keys_key_hash ON api_keys (key_hash);
CREATE INDEX idx_api_keys_tenant_id ON api_keys (tenant_id) WHERE revoked_at IS NULL;
