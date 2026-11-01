-- Per-tenant overrides of the vendor-metric caps (public.max_counter_* /
-- max_measure_* / max_metric_dims / max_dim_values_per_key). NULL means "use
-- the public server's global value", like the rate-limit overrides, so a
-- project with many checks can be given room without raising every tenant's
-- limit. Set with `argvio tenant config`; ingest reads them through the
-- tenant cache, so a change applies within its TTL, without a restart.
ALTER TABLE tenant_config
    ADD COLUMN max_counter_names           INTEGER CHECK (max_counter_names > 0),
    ADD COLUMN max_counter_labels_per_name INTEGER CHECK (max_counter_labels_per_name > 0),
    ADD COLUMN max_measure_names           INTEGER CHECK (max_measure_names > 0),
    ADD COLUMN max_measure_labels_per_name INTEGER CHECK (max_measure_labels_per_name > 0),
    ADD COLUMN max_metric_dims             INTEGER CHECK (max_metric_dims BETWEEN 1 AND 8),
    ADD COLUMN max_dim_values_per_key      INTEGER CHECK (max_dim_values_per_key > 0);
