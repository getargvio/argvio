-- Continuous aggregates for the dashboard queries the `metrics` server
-- issues most: command duration percentiles, error rate over time, and
-- command-frequency histograms. All three come off the same hourly rollup
-- of `traces` (one row per `cli.command.*` / `cli.subcommand.*` span),
-- since a command invocation span already carries exit_code + duration.
--
-- Percentiles use TimescaleDB Toolkit's percentile_agg (uddsketch-backed
-- approximate percentile), which is specifically designed to be
-- incrementally materializable inside a continuous aggregate — a plain
-- percentile_cont() ordered-set aggregate cannot be used here because
-- continuous aggregates can only incrementally combine aggregates that
-- support partial-state rollup, which percentile_cont does not.

CREATE MATERIALIZED VIEW cagg_command_stats_hourly
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 hour', time) AS bucket,
    count(*)                                          AS invocation_count,
    count(*) FILTER (WHERE exit_code IS DISTINCT FROM 0) AS error_count,
    percentile_agg(duration_ms)                        AS duration_percentiles
FROM traces
WHERE span_name LIKE 'cli.command.%' OR span_name LIKE 'cli.subcommand.%'
GROUP BY tenant_id, command_name, cli_version, os, arch, is_ci, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_command_stats_hourly',
    start_offset      => INTERVAL '3 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_command_stats_hourly_tenant_bucket
    ON cagg_command_stats_hourly (tenant_id, bucket DESC);
CREATE INDEX idx_cagg_command_stats_hourly_tenant_command_bucket
    ON cagg_command_stats_hourly (tenant_id, command_name, bucket DESC);

-- Daily rollup built on top of the hourly cagg (hierarchical continuous
-- aggregate) for wide-range dashboard queries that don't need hourly
-- resolution — avoids re-scanning raw `traces` rows for a 90-day view.
CREATE MATERIALIZED VIEW cagg_command_stats_daily
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 day', bucket) AS bucket,
    sum(invocation_count)         AS invocation_count,
    sum(error_count)              AS error_count,
    rollup(duration_percentiles)  AS duration_percentiles
FROM cagg_command_stats_hourly
GROUP BY tenant_id, command_name, cli_version, os, arch, is_ci, time_bucket('1 day', bucket)
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_command_stats_daily',
    start_offset      => INTERVAL '30 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_command_stats_daily_tenant_bucket
    ON cagg_command_stats_daily (tenant_id, bucket DESC);

-- Session/activation cohort rollup, sourced from logs (cli.session.started
-- events) for funnel/cohort-style time-bucketed counts distinct from
-- command-level stats.
CREATE MATERIALIZED VIEW cagg_session_activity_daily
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    cli_version,
    os,
    is_ci,
    time_bucket('1 day', time) AS bucket,
    count(*)                    AS session_count
FROM logs
WHERE event_name = 'cli.session.started'
GROUP BY tenant_id, cli_version, os, is_ci, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_session_activity_daily',
    start_offset      => INTERVAL '30 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_session_activity_daily_tenant_bucket
    ON cagg_session_activity_daily (tenant_id, bucket DESC);
