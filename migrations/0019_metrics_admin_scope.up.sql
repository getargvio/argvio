-- metrics_admin: an API key that may only replace a CLI's metric catalog
-- (PUT /v1/metric-definitions/{service}), so a vendor's CI can sync
-- argvio.metrics.yaml on release without a user session. It can't read
-- metrics, and a metrics_query key can't write.
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_scope_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_scope_check
    CHECK (scope IN ('public_ingest', 'metrics_query', 'metrics_admin'));
