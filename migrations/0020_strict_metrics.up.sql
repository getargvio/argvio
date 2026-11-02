-- strict_metrics: reject vendor metrics (cli.counter, cli.measure) that the
-- sending CLI's catalog doesn't declare, instead of accepting and folding
-- them. Off by default, so instrumenting needs no setup; a project turns it
-- on once its argvio.metrics.yaml is complete (`argvio tenant config
-- --strict-metrics`).
ALTER TABLE tenant_config ADD COLUMN strict_metrics BOOLEAN NOT NULL DEFAULT false;
