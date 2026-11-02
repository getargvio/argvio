ALTER TABLE tenant_config
    DROP COLUMN IF EXISTS max_counter_names,
    DROP COLUMN IF EXISTS max_counter_labels_per_name,
    DROP COLUMN IF EXISTS max_measure_names,
    DROP COLUMN IF EXISTS max_measure_labels_per_name,
    DROP COLUMN IF EXISTS max_metric_dims,
    DROP COLUMN IF EXISTS max_dim_values_per_key;
