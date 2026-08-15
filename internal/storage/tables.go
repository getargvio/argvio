package storage

// Hypertable names shared across policy reconciliation, retention sweeps,
// and the COPY-based writers.
const (
	TableTraces  = "traces"
	TableLogs    = "logs"
	TableMetrics = "metrics"
)
