-- The metric catalog: what each product metric of a CLI means
-- (docs/product-metrics-design.md, F3). One row per (tenant, CLI, metric),
-- the CLI being its OTel service.name. The source of truth is the
-- argvio.metrics.yaml file in the CLI's repository: `argvio metric apply`
-- replaces a service's rows with the file's definitions, and a metric that
-- left the file gets deprecated_at rather than being deleted, so dashboards
-- can flag it. Nothing edits a row field by field.
--
-- spec is the definition as the file states it (internal/catalog.Metric,
-- defaults filled in); applying a file leaves a row whose spec is unchanged
-- untouched. kind is copied out of it for filtering.
CREATE TABLE metric_definitions (
    tenant_id     UUID NOT NULL,
    service_name  TEXT NOT NULL,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('counter', 'duration', 'value', 'ratio')),
    spec          JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    updated_by    TEXT NOT NULL,
    deprecated_at TIMESTAMPTZ,

    PRIMARY KEY (tenant_id, service_name, name)
);
