-- Build footprint per session: executable size and startup overhead, reported
-- by CLI SDKs as cli.binary.size_bytes / cli.startup.duration_ms on
-- cli.session.started (once per process; size is a property of the build, not
-- of any one command).
--
-- Promoted to columns (rather than left in log_attributes) so a continuous
-- aggregate can sketch them. NULL means "not reported": SDKs for runtimes
-- with no single binary omit the size, and ingest also turns implausible
-- values into NULL (internal/otlp/usage.go), so percentile_agg() and
-- count(col) below only ever see real measurements. Both attributes are
-- allowed on cli.session.started only, so every other event leaves them NULL.
ALTER TABLE logs
    ADD COLUMN binary_size_bytes BIGINT,
    ADD COLUMN startup_ms        BIGINT;

-- A continuous aggregate's select list can't be altered in place, and
-- dropping cagg_session_activity_daily to add columns would lose rollup rows
-- older than the raw logs they could be rebuilt from. So, as with
-- cagg_command_resources_* (migration 0009), the footprint gets its own
-- rollup at the same grain as the session one.
--
-- Percentiles are uddsketch-backed percentile_agg, mergeable via rollup(), so
-- a figure over a whole query window is a true percentile, not an average of
-- daily ones. Min/max merge with min()/max(). The *_reported_count columns
-- are the sample sizes (sessions); the *_install_hll sketches count distinct
-- installs among those sessions. hyperloglog() ignores NULLs, so sessions
-- without an install id (anonymous tier) or without the measure add nothing.
--
-- Run
--   CALL refresh_continuous_aggregate('cagg_session_footprint_daily', NULL, NULL);
-- once after migrating to cover rows already in raw `logs`; those have no
-- footprint, so this only matters for rows ingested after the migration.
CREATE MATERIALIZED VIEW cagg_session_footprint_daily
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 day', time)  AS bucket,
    count(*)                    AS session_count,
    count(binary_size_bytes)    AS size_reported_count,
    min(binary_size_bytes)      AS size_min,
    max(binary_size_bytes)      AS size_max,
    percentile_agg(binary_size_bytes) AS size_percentiles,
    hyperloglog(8192, CASE WHEN binary_size_bytes IS NOT NULL THEN log_attributes ->> 'cli.install_id' END) AS size_install_hll,
    count(startup_ms)           AS startup_reported_count,
    percentile_agg(startup_ms)  AS startup_percentiles,
    hyperloglog(8192, CASE WHEN startup_ms IS NOT NULL THEN log_attributes ->> 'cli.install_id' END) AS startup_install_hll
FROM logs
WHERE event_name = 'cli.session.started'
GROUP BY tenant_id, cli_version, os, arch, is_ci, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_session_footprint_daily',
    start_offset      => INTERVAL '30 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_session_footprint_daily_tenant_bucket
    ON cagg_session_footprint_daily (tenant_id, bucket DESC);

SELECT add_retention_policy('cagg_session_footprint_daily', drop_after => INTERVAL '730 days');
