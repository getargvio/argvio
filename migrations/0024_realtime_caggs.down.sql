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
        EXECUTE format('ALTER MATERIALIZED VIEW %I SET (timescaledb.materialized_only = true)', v);
    END LOOP;
END
$$;
