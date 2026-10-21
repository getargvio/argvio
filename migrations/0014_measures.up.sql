-- Vendor-defined durations: the cli.measure metric, one generic histogram
-- (unit ms, delta) whose cli.measure.name / cli.measure.label attributes say
-- what is timed (docs/allowlist.md "Vendor measures").
--
-- Cardinality guard: the same capped vocabulary as counters (migration 0013),
-- in its own tables so the two instruments' caps are independent.
-- measure_names holds at most public.max_measure_names_per_tenant names,
-- measure_labels at most public.max_measure_labels_per_name labels per name;
-- ingest stores a value as-is only if it is listed or there is still room,
-- and "other" otherwise. last_seen lets `argvio retention sweep` drop values
-- no longer sent. measure_overflow counts the data points folded into
-- "other" per tenant, by which cap folded them.
--
-- No foreign key to tenants, like the other ingest-maintained tables.
CREATE TABLE measure_names (
    tenant_id    UUID NOT NULL,
    measure_name TEXT NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, measure_name)
);

CREATE TABLE measure_labels (
    tenant_id    UUID NOT NULL,
    measure_name TEXT NOT NULL,
    label        TEXT NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, measure_name, label)
);

CREATE TABLE measure_overflow (
    tenant_id      UUID PRIMARY KEY,
    folded_names   BIGINT NOT NULL,
    folded_labels  BIGINT NOT NULL,
    last_folded_at TIMESTAMPTZ NOT NULL
);

-- Rollups, at the grain of the counter ones. Ingest only accepts cli.measure
-- histograms with delta temporality and exactly the server's fixed bucket
-- bounds (schema/allowlist/v1.yaml, 11 bounds, so 12 buckets), which is what
-- makes their per-bucket counts summable: bkt_0..bkt_11 are those counts
-- added up, one column each since a continuous aggregate can't sum an array.
-- Percentiles are interpolated from them in Go (internal/storage/measure_percentiles.go),
-- which must agree with the bounds in the allowlist; changing them means a
-- new migration. sum_ms may undercount if some SDK omits the optional
-- histogram sum, and min_ms/max_ms are NULL if none reported them.
--
-- cli.measure was not accepted before this migration, so nothing needs a
-- backfill.
CREATE MATERIALIZED VIEW cagg_measures_hourly
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    datapoint_attributes ->> 'cli.measure.name'  AS measure_name,
    datapoint_attributes ->> 'cli.measure.label' AS measure_label,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 hour', time) AS bucket,
    sum(histogram_count)        AS measure_count,
    sum(histogram_sum)          AS sum_ms,
    min(histogram_min)          AS min_ms,
    max(histogram_max)          AS max_ms,
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
    count(*)                    AS datapoint_count
FROM metrics
WHERE metric_name = 'cli.measure' AND metric_type = 'histogram'
GROUP BY tenant_id, datapoint_attributes ->> 'cli.measure.name', datapoint_attributes ->> 'cli.measure.label',
         command_name, cli_version, os, arch, is_ci, bucket
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_measures_hourly',
    start_offset      => INTERVAL '3 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_measures_hourly_tenant_name_bucket
    ON cagg_measures_hourly (tenant_id, measure_name, bucket DESC);

SELECT add_retention_policy('cagg_measures_hourly', drop_after => INTERVAL '90 days');

CREATE MATERIALIZED VIEW cagg_measures_daily
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    measure_name,
    measure_label,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 day', bucket) AS bucket,
    sum(measure_count)           AS measure_count,
    sum(sum_ms)                  AS sum_ms,
    min(min_ms)                  AS min_ms,
    max(max_ms)                  AS max_ms,
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
    sum(datapoint_count)         AS datapoint_count
FROM cagg_measures_hourly
GROUP BY tenant_id, measure_name, measure_label, command_name, cli_version, os, arch, is_ci, time_bucket('1 day', bucket)
WITH NO DATA;

SELECT add_continuous_aggregate_policy('cagg_measures_daily',
    start_offset      => INTERVAL '30 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval  => INTERVAL '1 hour'
);

CREATE INDEX idx_cagg_measures_daily_tenant_bucket
    ON cagg_measures_daily (tenant_id, bucket DESC);
CREATE INDEX idx_cagg_measures_daily_tenant_name_bucket
    ON cagg_measures_daily (tenant_id, measure_name, bucket DESC);

SELECT add_retention_policy('cagg_measures_daily', drop_after => INTERVAL '730 days');
