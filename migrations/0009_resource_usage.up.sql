-- Process resource usage per command run: peak resident memory and CPU time,
-- reported by CLI SDKs as cli.process.peak_rss_bytes / cli.process.cpu_time_ms
-- on the cli.command.* span.
--
-- Promoted to columns (rather than left in span_attributes) so a continuous
-- aggregate can sketch them. NULL means "not reported": SDKs on platforms
-- that cannot measure omit the attribute, and ingest also turns zero,
-- negative and implausibly large values into NULL (internal/otlp/usage.go),
-- so percentile_agg() and count(col) below only ever see real measurements.
ALTER TABLE traces
    ADD COLUMN peak_rss_bytes BIGINT,
    ADD COLUMN cpu_time_ms    BIGINT;

-- A continuous aggregate's select list can't be altered in place, and
-- dropping cagg_command_stats_* to add columns would lose rollup rows older
-- than the 30 days of raw traces they could be rebuilt from. So, as with
-- cagg_exit_codes_hourly (migration 0005), resource usage gets its own pair
-- of rollups at the same grain as the command-stats ones.
--
-- Percentiles are uddsketch-backed percentile_agg, mergeable via rollup(),
-- exactly like duration_percentiles. rss_reported_count / cpu_reported_count
-- are the sample sizes behind them; invocation_count - reported is how many
-- runs did not report the measure.
--
-- Run
--   CALL refresh_continuous_aggregate('cagg_command_resources_hourly', NULL, NULL);
--   CALL refresh_continuous_aggregate('cagg_command_resources_daily', NULL, NULL);
-- once after migrating to cover rows already in raw `traces`; those have no
-- resource usage, so this only matters for rows ingested after the migration.
CREATE MATERIALIZED VIEW cagg_command_resources_hourly
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 hour', time) AS bucket,
    count(*)                    AS invocation_count,
    count(peak_rss_bytes)       AS rss_reported_count,
    count(cpu_time_ms)          AS cpu_reported_count,
    percentile_agg(peak_rss_bytes) AS rss_percentiles,
    percentile_agg(cpu_time_ms)    AS cpu_percentiles
FROM traces
WHERE span_name LIKE 'cli.command.%' OR span_name LIKE 'cli.subcommand.%'
GROUP BY tenant_id, command_name, cli_version, os, arch, is_ci, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_command_resources_hourly',
    start_offset      => INTERVAL '3 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_command_resources_hourly_tenant_bucket
    ON cagg_command_resources_hourly (tenant_id, bucket DESC);

SELECT add_retention_policy('cagg_command_resources_hourly', drop_after => INTERVAL '90 days');

CREATE MATERIALIZED VIEW cagg_command_resources_daily
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 day', bucket) AS bucket,
    sum(invocation_count)        AS invocation_count,
    sum(rss_reported_count)      AS rss_reported_count,
    sum(cpu_reported_count)      AS cpu_reported_count,
    rollup(rss_percentiles)      AS rss_percentiles,
    rollup(cpu_percentiles)      AS cpu_percentiles
FROM cagg_command_resources_hourly
GROUP BY tenant_id, command_name, cli_version, os, arch, is_ci, time_bucket('1 day', bucket)
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_command_resources_daily',
    start_offset      => INTERVAL '30 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_command_resources_daily_tenant_bucket
    ON cagg_command_resources_daily (tenant_id, bucket DESC);

SELECT add_retention_policy('cagg_command_resources_daily', drop_after => INTERVAL '730 days');
