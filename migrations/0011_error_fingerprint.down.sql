DROP INDEX IF EXISTS idx_logs_tenant_error_fingerprint_time;
ALTER TABLE logs DROP COLUMN IF EXISTS error_fingerprint;
