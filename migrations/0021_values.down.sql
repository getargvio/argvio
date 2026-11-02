-- Retried like migration 0015's drops: a refresh job the DROP cancels can
-- make it fail with "tuple concurrently deleted".
DO $$
DECLARE
    v       text;
    attempt int;
BEGIN
    FOREACH v IN ARRAY ARRAY['cagg_values_daily', 'cagg_values_hourly'] LOOP
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

ALTER TABLE tenant_config
    DROP COLUMN IF EXISTS max_value_names,
    DROP COLUMN IF EXISTS max_value_labels_per_name;
DROP TABLE IF EXISTS value_dim_values;
DROP TABLE IF EXISTS value_overflow;
DROP TABLE IF EXISTS value_labels;
DROP TABLE IF EXISTS value_names;
