-- First time each install used each feature or flag, derived at ingest from
-- cli.feature.activated (kind 'feature', named by cli.activation.feature) and
-- cli.command.invoked (kind 'flag', one row per name in cli.flag.names).
-- Time to discovery is first_at minus installs.first_seen.
--
-- Persisted rather than read from raw logs for the same reason as installs
-- (migration 0006): logs are only kept ~90 days, so an old install's "first"
-- use would look late, or be missing, once the real first use aged out.
--
-- Only basic-tier-or-above records carry cli.install_id, so anonymous-tier
-- sessions never appear here.
--
-- name is vendor-supplied (bounded to 64 chars by the allowlist). Rows are
-- purged together with their install by `argvio retention sweep`.
--
-- Like the signal hypertables: no foreign key to tenants or installs (tenant
-- existence is enforced upstream by the API-key lookup, and this is a hot
-- write path).
CREATE TABLE install_discoveries (
    tenant_id  UUID NOT NULL,
    install_id TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('feature', 'flag')),
    name       TEXT NOT NULL,
    first_at   TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, install_id, kind, name)
);

CREATE INDEX idx_install_discoveries_tenant_name ON install_discoveries (tenant_id, kind, name);
