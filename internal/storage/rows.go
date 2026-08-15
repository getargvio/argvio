// Package storage is the shared Postgres/Timescale data-access layer used
// by both servers: internal/storage's write path (this file + write.go) is
// used exclusively by the public server's ingest hot path, and its query
// builder (query.go) exclusively by the metrics server — see
// docs/architecture.md for why the two never share a code path even though
// they share a schema.
package storage

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Attrs marshals a Go attribute map to JSON once, so callers building many
// rows don't each re-marshal; also makes the empty-map case a single
// well-known constant.
type Attrs map[string]any

func (a Attrs) json() json.RawMessage {
	if len(a) == 0 {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(map[string]any(a))
	if err != nil {
		// Values reaching here have already passed allowlist.CheckAttribute,
		// so every value is a string/int64/bool/float64/[]string — all
		// trivially JSON-marshalable. A failure here is a programming
		// error, not a data error, so it's acceptable to fall back rather
		// than propagate an error through every row constructor.
		return json.RawMessage(`{}`)
	}
	return b
}

// TraceRow is one span, shaped for a direct CopyFrom into the traces
// hypertable (column order must match traceColumns in write.go).
type TraceRow struct {
	Time     time.Time
	TenantID uuid.UUID

	TraceID       string
	SpanID        string
	ParentSpanID  *string
	SpanName      string
	SpanKind      int16
	StartTime     time.Time
	EndTime       time.Time
	DurationMs    float64
	StatusCode    int16
	StatusMessage *string

	CLIVersion  *string
	OS          *string
	Arch        *string
	CommandName *string
	ExitCode    *int32
	IsCI        *bool
	ConsentTier string

	ScopeName          *string
	ScopeVersion       *string
	ResourceAttributes Attrs
	SpanAttributes     Attrs
}

// LogRow is one log record, shaped for CopyFrom into the logs hypertable.
type LogRow struct {
	Time     time.Time
	TenantID uuid.UUID

	TraceID        *string
	SpanID         *string
	SeverityNumber *int16
	SeverityText   *string
	EventName      string
	Body           *string // nil if stripped by consent tier enforcement

	CLIVersion  *string
	OS          *string
	Arch        *string
	CommandName *string
	ExitCode    *int32
	IsCI        *bool
	ConsentTier string

	ScopeName          *string
	ScopeVersion       *string
	ResourceAttributes Attrs
	LogAttributes      Attrs
}

// MetricRow is one metric data point, shaped for CopyFrom into the metrics
// hypertable. Value is used for sum/gauge; the Histogram* fields for
// histogram — exactly one of the two groups is populated per row.
type MetricRow struct {
	Time     time.Time
	TenantID uuid.UUID

	MetricName string
	MetricType string // sum | gauge | histogram
	Unit       *string

	Value *float64

	HistogramCount        *int64
	HistogramSum          *float64
	HistogramMin          *float64
	HistogramMax          *float64
	HistogramBucketBounds []float64
	HistogramBucketCounts []int64

	CLIVersion  *string
	OS          *string
	Arch        *string
	CommandName *string
	ExitCode    *int32
	IsCI        *bool
	ConsentTier string

	ScopeName           *string
	ScopeVersion        *string
	ResourceAttributes  Attrs
	DatapointAttributes Attrs
}
