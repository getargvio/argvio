-- Retried like migration 0015's drops: a refresh job the DROP cancels can
-- make it fail with "tuple concurrently deleted".
DO $$
DECLARE
    v       text;
    attempt int;
BEGIN
    FOREACH v IN ARRAY ARRAY['cagg_counters_daily', 'cagg_counters_hourly'] LOOP
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
DROP TABLE IF EXISTS counter_overflow;
DROP TABLE IF EXISTS counter_labels;
DROP TABLE IF EXISTS counter_names;
