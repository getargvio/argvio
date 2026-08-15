-- Columnar compression + retention policies. Global defaults sized to the
-- longest per-signal window we ship (see internal/config's storageDefaults
-- for the Go-side documentation of the same numbers). Tenants configured
-- with a *shorter* retention than these globals are pruned early by the
-- `cmd/admin retention sweep` job (docs/schema.md "Per-tenant retention"):
-- Timescale's own retention policy drops whole chunks, which are
-- hash-partitioned across many tenants, so it can only enforce one
-- (maximum) window per hypertable, not a true per-tenant one.

-- ---- traces: compress after 7 days, drop chunks after 30 days ----------
ALTER TABLE traces SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'tenant_id, command_name',
    timescaledb.compress_orderby   = 'time DESC, trace_id, span_id'
);
SELECT add_compression_policy('traces', compress_after => INTERVAL '7 days');
SELECT add_retention_policy('traces', drop_after => INTERVAL '30 days');

-- ---- logs: compress after 7 days, drop chunks after 90 days -----------
ALTER TABLE logs SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'tenant_id, event_name',
    timescaledb.compress_orderby   = 'time DESC, id'
);
SELECT add_compression_policy('logs', compress_after => INTERVAL '7 days');
SELECT add_retention_policy('logs', drop_after => INTERVAL '90 days');

-- ---- metrics: compress after 30 days, drop chunks after ~400 days -----
-- (kept longer: aggregated metric data points are cheap once compressed
-- and are the backbone of the continuous aggregates above.)
ALTER TABLE metrics SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'tenant_id, metric_name',
    timescaledb.compress_orderby   = 'time DESC, id'
);
SELECT add_compression_policy('metrics', compress_after => INTERVAL '30 days');
SELECT add_retention_policy('metrics', drop_after => INTERVAL '400 days');

-- Continuous aggregates get their own, longer retention independent of the
-- raw hypertables — the whole point of rollups is that they survive after
-- the raw rows they were built from are gone.
SELECT add_retention_policy('cagg_command_stats_hourly', drop_after => INTERVAL '90 days');
SELECT add_retention_policy('cagg_command_stats_daily', drop_after => INTERVAL '730 days');
SELECT add_retention_policy('cagg_session_activity_daily', drop_after => INTERVAL '730 days');
