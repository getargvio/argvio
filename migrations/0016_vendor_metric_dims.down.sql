ALTER TABLE measure_overflow DROP COLUMN IF EXISTS folded_dim_values;
ALTER TABLE counter_overflow DROP COLUMN IF EXISTS folded_dim_values;
DROP TABLE IF EXISTS measure_dim_values;
DROP TABLE IF EXISTS counter_dim_values;
