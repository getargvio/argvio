package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Writer is the public server's insert path: bulk CopyFrom per OTLP export
// request (one call per signal type per request, not per row), backed by
// its own connection pool so it never competes with the metrics server's
// query pool for connections (see internal/config.StorageConfig).
type Writer struct {
	pool *pgxpool.Pool
}

func NewWriter(pool *pgxpool.Pool) *Writer {
	return &Writer{pool: pool}
}

// Column names shared across the traces/logs/metrics tables.
const (
	colTime               = "time"
	colTenantID           = "tenant_id"
	colCLIVersion         = "cli_version"
	colArch               = "arch"
	colCommandName        = "command_name"
	colExitCode           = "exit_code"
	colIsCI               = "is_ci"
	colConsentTier        = "consent_tier"
	colScopeName          = "scope_name"
	colScopeVersion       = "scope_version"
	colResourceAttributes = "resource_attributes"
)

var traceColumns = []string{
	colTime, colTenantID, "trace_id", "span_id", "parent_span_id", "span_name", "span_kind",
	"start_time", "end_time", "duration_ms", "status_code", "status_message",
	colCLIVersion, "os", colArch, colCommandName, colExitCode, colIsCI, colConsentTier,
	colScopeName, colScopeVersion, colResourceAttributes, "span_attributes",
}

// InsertTraces bulk-inserts spans via COPY. Returns the number of rows
// written; a partial write cannot happen (COPY is all-or-nothing per call)
// so callers don't need to reconcile counts against len(rows).
func (w *Writer) InsertTraces(ctx context.Context, rows []TraceRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{
			r.Time, r.TenantID, r.TraceID, r.SpanID, r.ParentSpanID, r.SpanName, r.SpanKind,
			r.StartTime, r.EndTime, r.DurationMs, r.StatusCode, r.StatusMessage,
			r.CLIVersion, r.OS, r.Arch, r.CommandName, r.ExitCode, r.IsCI, r.ConsentTier,
			r.ScopeName, r.ScopeVersion, r.ResourceAttributes.json(), r.SpanAttributes.json(),
		}, nil
	})
	n, err := w.pool.CopyFrom(ctx, pgx.Identifier{TableTraces}, traceColumns, src)
	if err != nil {
		return n, fmt.Errorf("storage: insert traces: %w", err)
	}
	return n, nil
}

var logColumns = []string{
	colTime, colTenantID, "trace_id", "span_id", "severity_number", "severity_text", "event_name", "body",
	colCLIVersion, "os", colArch, colCommandName, colExitCode, colIsCI, colConsentTier,
	colScopeName, colScopeVersion, colResourceAttributes, "log_attributes",
}

func (w *Writer) InsertLogs(ctx context.Context, rows []LogRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{
			r.Time, r.TenantID, r.TraceID, r.SpanID, r.SeverityNumber, r.SeverityText, r.EventName, r.Body,
			r.CLIVersion, r.OS, r.Arch, r.CommandName, r.ExitCode, r.IsCI, r.ConsentTier,
			r.ScopeName, r.ScopeVersion, r.ResourceAttributes.json(), r.LogAttributes.json(),
		}, nil
	})
	n, err := w.pool.CopyFrom(ctx, pgx.Identifier{TableLogs}, logColumns, src)
	if err != nil {
		return n, fmt.Errorf("storage: insert logs: %w", err)
	}
	return n, nil
}

var metricColumns = []string{
	colTime, colTenantID, "metric_name", "metric_type", "unit", "value",
	"histogram_count", "histogram_sum", "histogram_min", "histogram_max",
	"histogram_bucket_bounds", "histogram_bucket_counts",
	colCLIVersion, "os", colArch, colCommandName, colExitCode, colIsCI, colConsentTier,
	colScopeName, colScopeVersion, colResourceAttributes, "datapoint_attributes",
}

func (w *Writer) InsertMetrics(ctx context.Context, rows []MetricRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{
			r.Time, r.TenantID, r.MetricName, r.MetricType, r.Unit, r.Value,
			r.HistogramCount, r.HistogramSum, r.HistogramMin, r.HistogramMax,
			r.HistogramBucketBounds, r.HistogramBucketCounts,
			r.CLIVersion, r.OS, r.Arch, r.CommandName, r.ExitCode, r.IsCI, r.ConsentTier,
			r.ScopeName, r.ScopeVersion, r.ResourceAttributes.json(), r.DatapointAttributes.json(),
		}, nil
	})
	n, err := w.pool.CopyFrom(ctx, pgx.Identifier{TableMetrics}, metricColumns, src)
	if err != nil {
		return n, fmt.Errorf("storage: insert metrics: %w", err)
	}
	return n, nil
}
