-- Vendor-defined counters: the cli.counter metric, one generic sum whose
-- cli.counter.name / cli.counter.label attributes say what is counted
-- (docs/allowlist.md "Vendor counters").
--
-- Cardinality guard. Name and label are vendor-chosen strings accepted at the
-- anonymous tier, so, as with error_reasons (migration 0012), each tenant has
-- a capped vocabulary: counter_names holds at most
-- public.max_counter_names_per_tenant names, counter_labels at most
-- public.max_counter_labels_per_name labels per name. Ingest stores a value
-- as-is only if it is listed or there is still room, and "other" otherwise.
-- last_seen lets `argvio retention sweep` drop values no longer sent; ingest
-- only moves it forward once a day per value. counter_overflow counts the data
-- points folded into "other" per tenant, by which cap folded them.
--
-- No foreign key to tenants, like the other ingest-maintained tables.
CREATE TABLE counter_names (
    tenant_id    UUID NOT NULL,
    counter_name TEXT NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, counter_name)
);

CREATE TABLE counter_labels (
    tenant_id    UUID NOT NULL,
    counter_name TEXT NOT NULL,
    label        TEXT NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, counter_name, label)
);

CREATE TABLE counter_overflow (
    tenant_id      UUID PRIMARY KEY,
    folded_names   BIGINT NOT NULL,
    folded_labels  BIGINT NOT NULL,
    last_folded_at TIMESTAMPTZ NOT NULL
);

-- Rollups, at the grain of the command-stats ones plus counter name and
-- label, which stay in datapoint_attributes (a continuous aggregate can group
-- by the expression). The allowlist only accepts delta-temporality,
-- non-negative cli.counter points, so summing `value` gives the count over a
-- bucket. datapoint_count is the number of data points behind it (roughly,
-- process exports).
--
-- These are the first rollups over `metrics`; nothing older needs a backfill,
-- since cli.counter was not accepted before this migration.
CREATE MATERIALIZED VIEW cagg_counters_hourly
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    datapoint_attributes ->> 'cli.counter.name'  AS counter_name,
    datapoint_attributes ->> 'cli.counter.label' AS counter_label,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 hour', time) AS bucket,
    sum(value)                  AS total,
    count(*)                    AS datapoint_count
FROM metrics
WHERE metric_name = 'cli.counter' AND metric_type = 'sum'
GROUP BY tenant_id, datapoint_attributes ->> 'cli.counter.name', datapoint_attributes ->> 'cli.counter.label',
         command_name, cli_version, os, arch, is_ci, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_counters_hourly',
    start_offset      => INTERVAL '3 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_counters_hourly_tenant_name_bucket
    ON cagg_counters_hourly (tenant_id, counter_name, bucket DESC);

SELECT add_retention_policy('cagg_counters_hourly', drop_after => INTERVAL '90 days');

CREATE MATERIALIZED VIEW cagg_counters_daily
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    counter_name,
    counter_label,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 day', bucket) AS bucket,
    sum(total)                   AS total,
    sum(datapoint_count)         AS datapoint_count
FROM cagg_counters_hourly
GROUP BY tenant_id, counter_name, counter_label, command_name, cli_version, os, arch, is_ci, time_bucket('1 day', bucket)
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_counters_daily',
    start_offset      => INTERVAL '30 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_counters_daily_tenant_bucket
    ON cagg_counters_daily (tenant_id, bucket DESC);
CREATE INDEX idx_cagg_counters_daily_tenant_name_bucket
    ON cagg_counters_daily (tenant_id, counter_name, bucket DESC);

SELECT add_retention_policy('cagg_counters_daily', drop_after => INTERVAL '730 days');
