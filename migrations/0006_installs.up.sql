-- Durable per-install state, derived at ingest from cli.session.started and
-- cli.command.completed logs. Raw logs are only kept ~90 days, so anything
-- that depends on "when did this install first appear" (first-run vs
-- returning, activation time, upgrades) can't be answered from them without
-- going wrong for older installs. One row per install keeps that answer.
--
-- Only basic-tier-or-above records carry cli.install_id, so anonymous-tier
-- sessions never appear here.
--
-- Rows are purged by `argvio retention sweep` once last_seen is older than
-- the tenant's logs retention (docs/journey-telemetry-design.md, decision 1);
-- an install returning after that window is counted as new.
--
-- Like the signal hypertables: no foreign key to tenants (tenant existence
-- is enforced upstream by the API-key lookup, and this is a hot write path).
CREATE TABLE installs (
    tenant_id        UUID NOT NULL,
    install_id       TEXT NOT NULL,

    first_seen       TIMESTAMPTZ NOT NULL,
    last_seen        TIMESTAMPTZ NOT NULL,
    first_version    TEXT,
    last_version     TEXT,
    last_os          TEXT,
    last_arch        TEXT,
    -- Vendor-reported via the SDK's install-method option; latest wins.
    install_method   TEXT,

    -- First cli.command.completed seen, and the first one with exit code 0.
    first_command_at TIMESTAMPTZ,
    first_success_at TIMESTAMPTZ,

    PRIMARY KEY (tenant_id, install_id)
);

CREATE INDEX idx_installs_tenant_first_seen ON installs (tenant_id, first_seen);
CREATE INDEX idx_installs_tenant_last_seen ON installs (tenant_id, last_seen);

-- Append-only log of version changes observed for an install. direction is
-- decided at ingest by comparing the two versions as semver; changes between
-- versions that don't parse as semver are not recorded.
CREATE TABLE install_version_changes (
    tenant_id    UUID NOT NULL,
    install_id   TEXT NOT NULL,
    at           TIMESTAMPTZ NOT NULL,
    from_version TEXT NOT NULL,
    to_version   TEXT NOT NULL,
    direction    TEXT NOT NULL CHECK (direction IN ('upgrade', 'downgrade')),

    PRIMARY KEY (tenant_id, install_id, at)
);

CREATE INDEX idx_install_version_changes_tenant_at ON install_version_changes (tenant_id, at);
