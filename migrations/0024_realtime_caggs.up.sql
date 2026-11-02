-- Real-time aggregation for every continuous aggregate the dashboard reads.
--
-- Since TimescaleDB 2.13 a continuous aggregate is created with
-- materialized_only = true, so a query only sees buckets the refresh policy
-- has materialized: those that end before now() - end_offset (1 hour here).
-- The open bucket was invisible, so data sent today reached the daily views
-- (OS/arch distribution, sessions started, active installs, retention, ...)
-- only at about 01:00 UTC the next day, and the hourly ones up to 2 hours
-- late (end_offset plus schedule_interval).
--
-- With materialized_only = false a query unions the materialized buckets with
-- an aggregate over the raw rows past the watermark. That tail is bounded by
-- one bucket plus end_offset, so the extra cost is small.
--
-- Recreating a continuous aggregate (as 0005, 0015 and 0022 did) resets this
-- flag; the integration test TestContinuousAggregatesAreRealTime fails when a
-- later migration forgets to set it again.

DO $$
DECLARE
    v text;
BEGIN
    FOREACH v IN ARRAY ARRAY[
        'cagg_command_stats_hourly',
        'cagg_command_stats_daily',
        'cagg_session_activity_daily',
        'cagg_exit_codes_hourly',
        'cagg_command_resources_hourly',
        'cagg_command_resources_daily',
        'cagg_session_footprint_daily',
        'cagg_counters_hourly',
        'cagg_counters_daily',
        'cagg_measures_hourly',
        'cagg_measures_daily',
        'cagg_values_hourly',
        'cagg_values_daily'
    ] LOOP
        EXECUTE format('ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = false)', v);
    END LOOP;
END
$$;
