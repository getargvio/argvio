-- Core signal tables: traces, logs, metrics. One row per span / log record /
-- metric data point — close to the OTLP data model (resource -> scope ->
-- record) rather than flattened, but with the columns we actually
-- filter/aggregate on promoted out of JSONB (docs/schema.md has the
-- rationale + full column-by-column notes).
--
-- Deliberately NOT using a foreign key to tenants(id) on these hypertables:
-- FK validation on every row would add lock/lookup overhead to the hottest
-- write path in the system, and tenant existence is already enforced
-- upstream by the public server's API-key lookup before any row is written.
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE EXTENSION IF NOT EXISTS timescaledb_toolkit;

-- ============================================================ traces =====
CREATE TABLE traces (
    time                TIMESTAMPTZ NOT NULL,
    tenant_id           UUID NOT NULL,
    id                  UUID NOT NULL DEFAULT gen_random_uuid(),

    trace_id            TEXT NOT NULL,
    span_id             TEXT NOT NULL,
    parent_span_id      TEXT,
    span_name           TEXT NOT NULL,
    span_kind           SMALLINT NOT NULL DEFAULT 0,
    start_time          TIMESTAMPTZ NOT NULL,
    end_time            TIMESTAMPTZ NOT NULL,
    duration_ms         DOUBLE PRECISION NOT NULL,
    status_code         SMALLINT NOT NULL DEFAULT 0, -- OTLP Status.StatusCode: 0 unset, 1 ok, 2 error
    status_message      TEXT,

    -- promoted filter/aggregate columns, shared shape across all 3 tables
    cli_version         TEXT,
    os                  TEXT,
    arch                TEXT,
    command_name        TEXT,
    exit_code           INTEGER,
    is_ci               BOOLEAN,
    consent_tier        TEXT NOT NULL,

    scope_name          TEXT,
    scope_version       TEXT,
    resource_attributes JSONB NOT NULL DEFAULT '{}',
    span_attributes     JSONB NOT NULL DEFAULT '{}',

    ingested_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, time, trace_id, span_id)
);

SELECT create_hypertable('traces', 'time', chunk_time_interval => INTERVAL '1 day');
-- Hash-partition by tenant_id in addition to time-chunking. With N buckets
-- this does not give 1 tenant : 1 partition (that doesn't scale), but it
-- does let the planner exclude chunks outside a queried tenant's hash
-- bucket for the always-tenant-scoped queries the metrics server issues.
-- 8 is a starting point for expected early tenant cardinality; revisit as
-- the tenant count grows (documented in docs/schema.md).
SELECT add_dimension('traces', 'tenant_id', number_partitions => 8);

CREATE INDEX idx_traces_tenant_time ON traces (tenant_id, time DESC);
CREATE INDEX idx_traces_tenant_command_time ON traces (tenant_id, command_name, time DESC);
CREATE INDEX idx_traces_tenant_trace_id ON traces (tenant_id, trace_id);
CREATE INDEX idx_traces_resource_attrs_gin ON traces USING GIN (resource_attributes);
CREATE INDEX idx_traces_span_attrs_gin ON traces USING GIN (span_attributes);

-- ============================================================== logs =====
CREATE TABLE logs (
    time                TIMESTAMPTZ NOT NULL,
    tenant_id           UUID NOT NULL,
    id                  UUID NOT NULL DEFAULT gen_random_uuid(),

    trace_id            TEXT,
    span_id             TEXT,
    severity_number     SMALLINT,
    severity_text       TEXT,
    event_name          TEXT NOT NULL,
    -- Nulled out by the consent-tier enforcement pipeline when the
    -- effective tier is below `full` (see internal/consent), even if the
    -- client sent it — never trust the client to have omitted it.
    body                TEXT,

    cli_version         TEXT,
    os                  TEXT,
    arch                TEXT,
    command_name        TEXT,
    exit_code           INTEGER,
    is_ci               BOOLEAN,
    consent_tier        TEXT NOT NULL,

    scope_name          TEXT,
    scope_version       TEXT,
    resource_attributes JSONB NOT NULL DEFAULT '{}',
    log_attributes      JSONB NOT NULL DEFAULT '{}',

    ingested_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, time, id)
);

SELECT create_hypertable('logs', 'time', chunk_time_interval => INTERVAL '1 day');
SELECT add_dimension('logs', 'tenant_id', number_partitions => 8);

CREATE INDEX idx_logs_tenant_time ON logs (tenant_id, time DESC);
CREATE INDEX idx_logs_tenant_event_time ON logs (tenant_id, event_name, time DESC);
CREATE INDEX idx_logs_tenant_command_time ON logs (tenant_id, command_name, time DESC);
CREATE INDEX idx_logs_resource_attrs_gin ON logs USING GIN (resource_attributes);
CREATE INDEX idx_logs_log_attrs_gin ON logs USING GIN (log_attributes);

-- =========================================================== metrics =====
CREATE TABLE metrics (
    time                    TIMESTAMPTZ NOT NULL,
    tenant_id               UUID NOT NULL,
    id                      UUID NOT NULL DEFAULT gen_random_uuid(),

    metric_name             TEXT NOT NULL,
    metric_type             TEXT NOT NULL CHECK (metric_type IN ('sum', 'gauge', 'histogram')),
    unit                    TEXT,

    -- populated for sum/gauge; NULL for histogram
    value                   DOUBLE PRECISION,

    -- populated for histogram; NULL for sum/gauge
    histogram_count         BIGINT,
    histogram_sum           DOUBLE PRECISION,
    histogram_min           DOUBLE PRECISION,
    histogram_max           DOUBLE PRECISION,
    histogram_bucket_bounds DOUBLE PRECISION[],
    histogram_bucket_counts BIGINT[],

    cli_version             TEXT,
    os                      TEXT,
    arch                    TEXT,
    command_name            TEXT,
    exit_code               INTEGER,
    is_ci                   BOOLEAN,
    consent_tier            TEXT NOT NULL,

    scope_name              TEXT,
    scope_version           TEXT,
    resource_attributes     JSONB NOT NULL DEFAULT '{}',
    datapoint_attributes    JSONB NOT NULL DEFAULT '{}',

    ingested_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, time, id)
);

SELECT create_hypertable('metrics', 'time', chunk_time_interval => INTERVAL '1 day');
SELECT add_dimension('metrics', 'tenant_id', number_partitions => 8);

CREATE INDEX idx_metrics_tenant_time ON metrics (tenant_id, time DESC);
CREATE INDEX idx_metrics_tenant_name_time ON metrics (tenant_id, metric_name, time DESC);
CREATE INDEX idx_metrics_resource_attrs_gin ON metrics USING GIN (resource_attributes);
CREATE INDEX idx_metrics_datapoint_attrs_gin ON metrics USING GIN (datapoint_attributes);
