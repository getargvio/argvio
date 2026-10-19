-- Failure-cause grouping for cli.error.raised: a fingerprint of the
-- normalized stack (innermost frames) or, without one, of the normalized
-- message (internal/errfp, docs/schema.md "Error fingerprints").
--
-- Computed once at ingest rather than in SQL at query time: the normalizer
-- is a stack-format-aware parser that is far cheaper to run once per event
-- in Go than per query in regexp_replace(), and grouping by a stored column
-- lets the index below serve the query. The price is that the algorithm is
-- fixed for stored rows: a change to it (errfp.Version) only applies to
-- events ingested afterwards, and rows ingested before this migration have
-- no fingerprint and don't appear in /v1/metrics/error-fingerprints.
--
-- NULL for every other event, and for error events with neither a message
-- nor a stack (both are full-tier attributes, so below full there is
-- nothing to fingerprint).
ALTER TABLE logs ADD COLUMN error_fingerprint TEXT;

CREATE INDEX idx_logs_tenant_error_fingerprint_time
    ON logs (tenant_id, error_fingerprint, time DESC)
    WHERE error_fingerprint IS NOT NULL;
