DROP MATERIALIZED VIEW IF EXISTS cagg_exit_codes_hourly;

-- Restore the 0003 definition of cagg_session_activity_daily (no arch, no
-- install sketch), plus its 0004 retention policy.
DROP MATERIALIZED VIEW IF EXISTS cagg_session_activity_daily;

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

SELECT add_retention_policy('cagg_session_activity_daily', drop_after => INTERVAL '730 days');
