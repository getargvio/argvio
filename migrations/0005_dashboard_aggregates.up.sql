-- Rollups backing the dashboard's second wave of aggregate endpoints:
--
-- 1. cagg_session_activity_daily is rebuilt to add `arch` (so session
--    counts can be broken down/filtered by arch like every other
--    dimension) and `install_hll`, a HyperLogLog sketch of
--    cli.install_id. Sketches union correctly across days via rollup(), so
--    weekly/monthly active installs are true distinct counts rather than
--    sums of daily ones. hyperloglog() ignores NULLs, i.e. anonymous-tier
--    sessions (which never carry cli.install_id) count toward
--    session_count but not toward active installs.
--
--    A continuous aggregate's GROUP BY can't be altered in place, so this
--    drops and recreates it. The recreated view is empty until refreshed:
--    the refresh policy below backfills the last 30 days; run
--      CALL refresh_continuous_aggregate('cagg_session_activity_daily', NULL, NULL);
--    once after migrating to backfill everything still in raw `logs`.
--    Rollup rows older than raw log retention cannot be rebuilt.
--
-- 2. cagg_exit_codes_hourly: invocation counts per exit_code, which the
--    command-stats rollups collapse into a single error_count. Hourly only
--    — the metrics API re-buckets it to day/week/month at query time, and
--    MetricsConfig.max_time_range_span keeps that scan bounded.

DROP MATERIALIZED VIEW cagg_session_activity_daily;

CREATE MATERIALIZED VIEW cagg_session_activity_daily
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 day', time) AS bucket,
    count(*)                    AS session_count,
    hyperloglog(8192, log_attributes ->> 'cli.install_id') AS install_hll
FROM logs
WHERE event_name = 'cli.session.started'
GROUP BY tenant_id, cli_version, os, arch, is_ci, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_session_activity_daily',
    start_offset      => INTERVAL '30 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_session_activity_daily_tenant_bucket
    ON cagg_session_activity_daily (tenant_id, bucket DESC);

SELECT add_retention_policy('cagg_session_activity_daily', drop_after => INTERVAL '730 days');

CREATE MATERIALIZED VIEW cagg_exit_codes_hourly
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    exit_code,
    time_bucket('1 hour', time) AS bucket,
    count(*)                    AS invocation_count
FROM traces
WHERE span_name LIKE 'cli.command.%' OR span_name LIKE 'cli.subcommand.%'
GROUP BY tenant_id, command_name, cli_version, os, arch, is_ci, exit_code, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_exit_codes_hourly',
    start_offset      => INTERVAL '3 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_exit_codes_hourly_tenant_bucket
    ON cagg_exit_codes_hourly (tenant_id, bucket DESC);

SELECT add_retention_policy('cagg_exit_codes_hourly', drop_after => INTERVAL '90 days');
