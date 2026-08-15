SELECT remove_retention_policy('cagg_session_activity_daily', if_exists => true);
SELECT remove_retention_policy('cagg_command_stats_daily', if_exists => true);
SELECT remove_retention_policy('cagg_command_stats_hourly', if_exists => true);

SELECT remove_retention_policy('metrics', if_exists => true);
SELECT remove_compression_policy('metrics', if_exists => true);
ALTER TABLE metrics SET (timescaledb.compress = false);

SELECT remove_retention_policy('logs', if_exists => true);
SELECT remove_compression_policy('logs', if_exists => true);
ALTER TABLE logs SET (timescaledb.compress = false);

SELECT remove_retention_policy('traces', if_exists => true);
SELECT remove_compression_policy('traces', if_exists => true);
ALTER TABLE traces SET (timescaledb.compress = false);
