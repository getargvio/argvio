-- The Product dashboard's saved layout, per tenant and CLI: which cards a
-- team pinned, in which order and type. Metric definitions come from the
-- CLI's argvio.metrics.yaml; the layout is the one thing the dashboard owns.
-- Replaced whole by PUT /v1/dashboard/cards/{service} (admin role). No rows
-- for a CLI means "the default layout generated from its catalog".
CREATE TABLE dashboard_cards (
    tenant_id    UUID NOT NULL,
    service_name TEXT NOT NULL,
    position     INTEGER NOT NULL CHECK (position >= 0),
    metric       TEXT NOT NULL,
    card_type    TEXT NOT NULL CHECK (card_type IN ('kpi', 'trend', 'breakdown', 'distribution', 'reach')),
    options      JSONB NOT NULL DEFAULT '{}',
    updated_at   TIMESTAMPTZ NOT NULL,
    updated_by   TEXT NOT NULL,

    PRIMARY KEY (tenant_id, service_name, position)
);
