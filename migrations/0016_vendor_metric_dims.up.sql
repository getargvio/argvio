-- Dimensions of vendor counters and measures (cli.dim.<key>, docs/allowlist.md
-- "Dimensions"). Ingest folds a data point's dimensions into one canonical
-- tuple, "check=bola;severity=high", stored as its label, so the rollups and
-- the label vocabularies (counter_labels, measure_labels) need no change: the
-- *_labels_per_name caps bound the tuples per name.
--
-- These tables add the second guard: the distinct values of each dimension
-- key per name, capped at public.max_dim_values_per_key. A value past the cap
-- is stored as "other" inside the tuple ("check=other;severity=high"), so
-- the other dimensions keep their breakdown. Maintained and swept like the
-- label vocabularies; folded_dim_values counts the data points with at least
-- one value folded.

CREATE TABLE counter_dim_values (
    tenant_id    UUID NOT NULL,
    counter_name TEXT NOT NULL,
    dim_key      TEXT NOT NULL,
    dim_value    TEXT NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, counter_name, dim_key, dim_value)
);

CREATE TABLE measure_dim_values (
    tenant_id    UUID NOT NULL,
    measure_name TEXT NOT NULL,
    dim_key      TEXT NOT NULL,
    dim_value    TEXT NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, measure_name, dim_key, dim_value)
);

ALTER TABLE counter_overflow ADD COLUMN folded_dim_values BIGINT NOT NULL DEFAULT 0;
ALTER TABLE measure_overflow ADD COLUMN folded_dim_values BIGINT NOT NULL DEFAULT 0;
