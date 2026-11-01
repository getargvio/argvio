-- Per-run value distributions: the cli.value metric (docs/allowlist.md
-- "Vendor values"), a delta histogram of non-time quantities a CLI records
-- once per run (findings per scan, operations scanned, spec size), with
-- cli.value.name, an optional label or dimension tuple, and cli.value.unit
-- (1, By or %). Its fixed bounds start with 0, so the first bucket counts the
-- runs that recorded nothing: that is what "share of scans that found
-- something" (hit rate) is computed from.
--
-- Vocabulary, overflow and dimension tables mirror measures (migrations 0014,
-- 0016), with their own caps (public.max_value_names_per_tenant,
-- public.max_value_labels_per_name, overridable per tenant).

CREATE TABLE value_names (
    tenant_id  UUID NOT NULL,
    value_name TEXT NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, value_name)
);

CREATE TABLE value_labels (
    tenant_id  UUID NOT NULL,
    value_name TEXT NOT NULL,
    label      TEXT NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, value_name, label)
);

CREATE TABLE value_overflow (
    tenant_id         UUID PRIMARY KEY,
    folded_names      BIGINT NOT NULL,
    folded_labels     BIGINT NOT NULL,
    folded_dim_values BIGINT NOT NULL DEFAULT 0,
    last_folded_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE value_dim_values (
    tenant_id  UUID NOT NULL,
    value_name TEXT NOT NULL,
    dim_key    TEXT NOT NULL,
    dim_value  TEXT NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, value_name, dim_key, dim_value)
);

ALTER TABLE tenant_config
    ADD COLUMN max_value_names           INTEGER CHECK (max_value_names > 0),
    ADD COLUMN max_value_labels_per_name INTEGER CHECK (max_value_labels_per_name > 0);

-- Rollups, like cagg_measures_* with service_name (migration 0015) and the
-- unit, one column per bucket of the bounds
-- [0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 5000, 10000, 100000].
CREATE MATERIALIZED VIEW cagg_values_hourly
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    datapoint_attributes ->> 'cli.value.name'  AS value_name,
    datapoint_attributes ->> 'cli.value.label' AS value_label,
    coalesce(datapoint_attributes ->> 'cli.value.unit', '1') AS value_unit,
    resource_attributes ->> 'service.name' AS service_name,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 hour', time) AS bucket,
    sum(histogram_count)        AS value_count,
    sum(histogram_sum)          AS value_sum,
    min(histogram_min)          AS value_min,
    max(histogram_max)          AS value_max,
    sum(histogram_bucket_counts[1]) AS bkt_0,
    sum(histogram_bucket_counts[2]) AS bkt_1,
    sum(histogram_bucket_counts[3]) AS bkt_2,
    sum(histogram_bucket_counts[4]) AS bkt_3,
    sum(histogram_bucket_counts[5]) AS bkt_4,
    sum(histogram_bucket_counts[6]) AS bkt_5,
    sum(histogram_bucket_counts[7]) AS bkt_6,
    sum(histogram_bucket_counts[8]) AS bkt_7,
    sum(histogram_bucket_counts[9]) AS bkt_8,
    sum(histogram_bucket_counts[10]) AS bkt_9,
    sum(histogram_bucket_counts[11]) AS bkt_10,
    sum(histogram_bucket_counts[12]) AS bkt_11,
    sum(histogram_bucket_counts[13]) AS bkt_12,
    sum(histogram_bucket_counts[14]) AS bkt_13,
    sum(histogram_bucket_counts[15]) AS bkt_14,
    count(*)                    AS datapoint_count
FROM metrics
WHERE metric_name = 'cli.value' AND metric_type = 'histogram'
GROUP BY tenant_id, datapoint_attributes ->> 'cli.value.name', datapoint_attributes ->> 'cli.value.label',
         coalesce(datapoint_attributes ->> 'cli.value.unit', '1'), resource_attributes ->> 'service.name',
         command_name, cli_version, os, arch, is_ci, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_values_hourly',
    start_offset      => INTERVAL '3 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_values_hourly_tenant_name_bucket
    ON cagg_values_hourly (tenant_id, value_name, bucket DESC);

SELECT add_retention_policy('cagg_values_hourly', drop_after => INTERVAL '90 days');

CREATE MATERIALIZED VIEW cagg_values_daily
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    value_name,
    value_label,
    value_unit,
    service_name,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 day', bucket) AS bucket,
    sum(value_count)             AS value_count,
    sum(value_sum)               AS value_sum,
    min(value_min)               AS value_min,
    max(value_max)               AS value_max,
    sum(bkt_0) AS bkt_0,
    sum(bkt_1) AS bkt_1,
    sum(bkt_2) AS bkt_2,
    sum(bkt_3) AS bkt_3,
    sum(bkt_4) AS bkt_4,
    sum(bkt_5) AS bkt_5,
    sum(bkt_6) AS bkt_6,
    sum(bkt_7) AS bkt_7,
    sum(bkt_8) AS bkt_8,
    sum(bkt_9) AS bkt_9,
    sum(bkt_10) AS bkt_10,
    sum(bkt_11) AS bkt_11,
    sum(bkt_12) AS bkt_12,
    sum(bkt_13) AS bkt_13,
    sum(bkt_14) AS bkt_14,
    sum(datapoint_count)         AS datapoint_count
FROM cagg_values_hourly
GROUP BY tenant_id, value_name, value_label, value_unit, service_name, command_name, cli_version, os, arch, is_ci, time_bucket('1 day', bucket)
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_values_daily',
    start_offset      => INTERVAL '30 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_values_daily_tenant_bucket
    ON cagg_values_daily (tenant_id, bucket DESC);
CREATE INDEX idx_cagg_values_daily_tenant_name_bucket
    ON cagg_values_daily (tenant_id, value_name, bucket DESC);
CREATE INDEX idx_cagg_values_daily_tenant_service_bucket
    ON cagg_values_daily (tenant_id, service_name, bucket DESC);

SELECT add_retention_policy('cagg_values_daily', drop_after => INTERVAL '730 days');
