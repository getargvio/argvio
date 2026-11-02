DELETE FROM api_keys WHERE scope = 'metrics_admin';
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_scope_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scope_check
    CHECK (scope IN ('public_ingest', 'metrics_query'));
