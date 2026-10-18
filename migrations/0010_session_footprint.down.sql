DROP MATERIALIZED VIEW IF EXISTS cagg_session_footprint_daily;
ALTER TABLE logs
    DROP COLUMN IF EXISTS startup_ms,
    DROP COLUMN IF EXISTS binary_size_bytes;
