-- Scope vendor counters and measures by CLI. A tenant may ship several CLIs,
-- each identified by the OTel resource attribute service.name (both SDKs set
-- it to the CLI's name), so the rollups of migrations 0013 and 0014 gain a
-- service_name column; without it two CLIs sending the same counter name are
-- summed together. The rollups are otherwise unchanged.
--
-- A continuous aggregate can't gain a GROUP BY column in place, so they are
-- dropped and recreated. Their policies only refresh the last 3 (hourly) and
-- 30 (daily) days, so run
--   CALL refresh_continuous_aggregate('cagg_counters_hourly', NULL, NULL);
--   CALL refresh_continuous_aggregate('cagg_counters_daily', NULL, NULL);
--   CALL refresh_continuous_aggregate('cagg_measures_hourly', NULL, NULL);
--   CALL refresh_continuous_aggregate('cagg_measures_daily', NULL, NULL);
-- once after migrating (in that order, outside a transaction) to rebuild
-- older buckets from the raw `metrics` hypertable, whose retention (400 days)
-- covers everything sent since cli.counter and cli.measure were accepted.
--
-- Vocabulary caps (counter_names, measure_names, ...) stay per tenant: they
-- protect the tenant's storage, whichever of its CLIs fills it.

-- Dropping a continuous aggregate deletes its refresh job. If that job is
-- running, TimescaleDB cancels it and the DROP can fail with "tuple
-- concurrently deleted" on the job's catalog row (right after 0013 and 0014
-- on a fresh database, the first refreshes start immediately). The job is
-- cancelled by then, so the DROP is retried, for that error only.
DO $$
DECLARE
    v       text;
    attempt int;
BEGIN
    FOREACH v IN ARRAY ARRAY['cagg_counters_daily', 'cagg_counters_hourly', 'cagg_measures_daily', 'cagg_measures_hourly'] LOOP
        FOR attempt IN 1..50 LOOP
            BEGIN
                EXECUTE format('DROP MATERIALIZED VIEW IF EXISTS %I', v);
                EXIT;
            EXCEPTION WHEN internal_error THEN
                IF attempt = 50 OR SQLERRM <> 'tuple concurrently deleted' THEN
                    RAISE;
                END IF;
                PERFORM pg_sleep(0.1);
            END;
        END LOOP;
    END LOOP;
END
$$;

CREATE MATERIALIZED VIEW cagg_counters_hourly
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    datapoint_attributes ->> 'cli.counter.name'  AS counter_name,
    datapoint_attributes ->> 'cli.counter.label' AS counter_label,
    resource_attributes ->> 'service.name' AS service_name,
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
         resource_attributes ->> 'service.name',
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
    service_name,
    command_name,
    cli_version,
    os,
    arch,
    is_ci,
    time_bucket('1 day', bucket) AS bucket,
    sum(total)                   AS total,
    sum(datapoint_count)         AS datapoint_count
FROM cagg_counters_hourly
GROUP BY tenant_id, counter_name, counter_label, service_name, command_name, cli_version, os, arch, is_ci, time_bucket('1 day', bucket)
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

CREATE INDEX idx_cagg_counters_daily_tenant_service_bucket
    ON cagg_counters_daily (tenant_id, service_name, bucket DESC);

CREATE MATERIALIZED VIEW cagg_measures_hourly
WITH (timescaledb.continuous) AS
SELECT
    tenant_id,
    datapoint_attributes ->> 'cli.measure.name'  AS measure_name,
    datapoint_attributes ->> 'cli.measure.label' AS measure_label,
    resource_attributes ->> 'service.name' AS service_name,
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
         resource_attributes ->> 'service.name',
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
    service_name,
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
GROUP BY tenant_id, measure_name, measure_label, service_name, command_name, cli_version, os, arch, is_ci, time_bucket('1 day', bucket)
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

CREATE INDEX idx_cagg_measures_daily_tenant_service_bucket
    ON cagg_measures_daily (tenant_id, service_name, bucket DESC);
