-- Cardinality guard for cli.error.reason, a vendor-assigned identifier from a
-- fixed vocabulary (docs/allowlist.md). error_reasons is the vocabulary each
-- tenant has been admitted so far: ingest stores a reason as-is only if it is
-- listed here or there is still room under public.max_error_reasons_per_tenant,
-- and stores "other" otherwise, so a vendor bug that puts user input in the
-- field can't create unbounded distinct values.
--
-- last_seen lets `argvio retention sweep` drop reasons no longer sent (on the
-- logs retention window), so a burst of bad values doesn't hold the cap
-- forever. Ingest only moves it forward once a day per reason, to keep the
-- common path read-only.
--
-- error_reason_overflow counts the records whose reason was folded into
-- "other" because the cap was reached, per tenant.
--
-- No foreign key to tenants, like the other ingest-maintained tables.
CREATE TABLE error_reasons (
    tenant_id  UUID NOT NULL,
    reason     TEXT NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, reason)
);

CREATE TABLE error_reason_overflow (
    tenant_id      UUID PRIMARY KEY,
    folded_count   BIGINT NOT NULL,
    last_folded_at TIMESTAMPTZ NOT NULL
);
