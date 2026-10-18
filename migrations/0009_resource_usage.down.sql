DROP MATERIALIZED VIEW IF EXISTS cagg_command_resources_daily;
DROP MATERIALIZED VIEW IF EXISTS cagg_command_resources_hourly;
ALTER TABLE traces
    DROP COLUMN IF EXISTS cpu_time_ms,
    DROP COLUMN IF EXISTS peak_rss_bytes;
