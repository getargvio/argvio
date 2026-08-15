package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// QueryBuilder is the metrics server's entire read path. Every method takes
// a TenantScope (see scope.go) so tenant isolation is structural, not
// convention — there is no method here that can run without one.
//
// Uses its own pgxpool.Pool, independent from the public server's Writer
// pool (internal/config.StorageConfig.MetricsPool), so a slow analytical
// query here can never starve ingest of connections.
type QueryBuilder struct {
	pool *pgxpool.Pool
}

func NewQueryBuilder(pool *pgxpool.Pool) *QueryBuilder {
	return &QueryBuilder{pool: pool}
}

// TimeRange bounds every query in this package — the metrics server's HTTP
// layer is responsible for rejecting a range wider than
// MetricsConfig.MaxTimeRangeSpan before it ever reaches here (storage has
// no config dependency by design).
type TimeRange struct {
	From time.Time
	To   time.Time
}

// Filters covers every dimension the metrics API's OpenAPI spec exposes as
// a query parameter. Every field is optional (nil/zero = unfiltered).
type Filters struct {
	Time TimeRange

	CLIVersion  *string
	OS          *string
	Arch        *string
	CommandName *string
	ExitCode    *int32
	IsCI        *bool

	// Attrs applies exact-match filters against the record-level JSONB
	// attributes column (span_attributes/log_attributes/
	// datapoint_attributes depending on the query), e.g.
	// {"cli.ci_provider": "github_actions"}. Keys and values are always
	// bound as query parameters (never string-concatenated into SQL), so
	// there is no injection surface via attribute filter values regardless
	// of what a tenant's dashboard user types in.
	Attrs map[string]string

	Limit  int
	Offset int
}

// predicateBuilder accumulates "AND col = $n"-style clauses with correctly
// numbered placeholders, always seeded with the tenant_id predicate from a
// TenantScope so every query built through it is tenant-scoped by
// construction.
type predicateBuilder struct {
	clauses []string
	args    []any
}

func newScopedPredicates(scope TenantScope, tenantCol string) *predicateBuilder {
	pb := &predicateBuilder{}
	pb.add(tenantCol+" = $%d", scope.tenantID)
	return pb
}

func (pb *predicateBuilder) add(clauseFmt string, arg any) {
	pb.args = append(pb.args, arg)
	pb.clauses = append(pb.clauses, fmt.Sprintf(clauseFmt, len(pb.args)))
}

func (pb *predicateBuilder) addIfNotNilStr(col string, v *string) {
	if v != nil {
		pb.add(col+" = $%d", *v)
	}
}

func (pb *predicateBuilder) addIfNotNilI32(col string, v *int32) {
	if v != nil {
		pb.add(col+" = $%d", *v)
	}
}

func (pb *predicateBuilder) addIfNotNilBool(col string, v *bool) {
	if v != nil {
		pb.add(col+" = $%d", *v)
	}
}

func (pb *predicateBuilder) addAttrEquals(jsonCol string, attrs map[string]string) {
	for k, v := range attrs {
		pb.args = append(pb.args, k, v)
		keyIdx := len(pb.args) - 1
		valIdx := len(pb.args)
		pb.clauses = append(pb.clauses, fmt.Sprintf("(%s ->> $%d) = $%d", jsonCol, keyIdx, valIdx))
	}
}

func (pb *predicateBuilder) where() string {
	return strings.Join(pb.clauses, " AND ")
}

// nextPlaceholder returns the next unused $N, for callers appending
// LIMIT/OFFSET or other trailing params after the WHERE clause is built.
func (pb *predicateBuilder) nextPlaceholder(arg any) string {
	pb.args = append(pb.args, arg)
	return fmt.Sprintf("$%d", len(pb.args))
}

// --- raw span listing -------------------------------------------------

// TraceSummary is one row from a ListTraces call — a read projection of
// the traces hypertable, not the write-side TraceRow.
type TraceSummary struct {
	Time          time.Time `json:"time"`
	TraceID       string    `json:"trace_id"`
	SpanID        string    `json:"span_id"`
	SpanName      string    `json:"span_name"`
	DurationMs    float64   `json:"duration_ms"`
	StatusCode    int16     `json:"status_code"`
	StatusMessage *string   `json:"status_message,omitempty"`
	CommandName   *string   `json:"command_name,omitempty"`
	ExitCode      *int32    `json:"exit_code,omitempty"`
	CLIVersion    *string   `json:"cli_version,omitempty"`
	OS            *string   `json:"os,omitempty"`
	Arch          *string   `json:"arch,omitempty"`
	IsCI          *bool     `json:"is_ci,omitempty"`
	ConsentTier   string    `json:"consent_tier"`
}

// ListTraces returns raw spans matching f, newest first, paginated. This is
// the drill-down query behind the aggregates — use LatencyPercentiles/
// ErrorRateSeries/CommandFrequency for dashboard-scale queries instead of
// paging through this for large ranges.
func (q *QueryBuilder) ListTraces(ctx context.Context, scope TenantScope, f Filters) ([]TraceSummary, error) {
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("time >= $%d", f.Time.From)
	pb.add("time < $%d", f.Time.To)
	pb.addIfNotNilStr("cli_version", f.CLIVersion)
	pb.addIfNotNilStr("os", f.OS)
	pb.addIfNotNilStr("arch", f.Arch)
	pb.addIfNotNilStr("command_name", f.CommandName)
	pb.addIfNotNilI32("exit_code", f.ExitCode)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	pb.addAttrEquals("span_attributes", f.Attrs)

	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT time, trace_id, span_id, span_name, duration_ms, status_code, status_message,
       command_name, exit_code, cli_version, os, arch, is_ci, consent_tier
FROM traces
WHERE %s
ORDER BY time DESC
LIMIT %s OFFSET %s`, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list traces: %w", err)
	}
	defer rows.Close()

	var out []TraceSummary
	for rows.Next() {
		var r TraceSummary
		if err := rows.Scan(&r.Time, &r.TraceID, &r.SpanID, &r.SpanName, &r.DurationMs, &r.StatusCode, &r.StatusMessage,
			&r.CommandName, &r.ExitCode, &r.CLIVersion, &r.OS, &r.Arch, &r.IsCI, &r.ConsentTier); err != nil {
			return nil, fmt.Errorf("storage: scan trace: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- command stats (percentiles / error rate / frequency), off the ---
// --- continuous aggregates, not raw traces ----------------------------

// Bucket selects which continuous aggregate backs a command-stats query.
// Only hour/day are supported — those are the two materialized rollups
// (migrations/0003_continuous_aggregates.up.sql); anything finer would
// require scanning raw traces, which ListTraces is for.
type Bucket string

const (
	BucketHour Bucket = "hour"
	BucketDay  Bucket = "day"
)

func (b Bucket) table() (string, error) {
	switch b {
	case BucketHour:
		return "cagg_command_stats_hourly", nil
	case BucketDay:
		return "cagg_command_stats_daily", nil
	default:
		return "", fmt.Errorf("storage: unsupported bucket %q (want %q or %q)", b, BucketHour, BucketDay)
	}
}

type commandStatRow struct {
	Bucket          time.Time
	CommandName     string
	InvocationCount int64
	ErrorCount      int64
	P50             *float64
	P95             *float64
	P99             *float64
}

func (q *QueryBuilder) commandStats(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]commandStatRow, error) {
	table, err := bucket.table()
	if err != nil {
		return nil, err
	}

	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("bucket >= $%d", f.Time.From)
	pb.add("bucket < $%d", f.Time.To)
	pb.addIfNotNilStr("command_name", f.CommandName)
	pb.addIfNotNilStr("cli_version", f.CLIVersion)
	pb.addIfNotNilStr("os", f.OS)
	pb.addIfNotNilStr("arch", f.Arch)
	pb.addIfNotNilBool("is_ci", f.IsCI)

	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT bucket, command_name,
       sum(invocation_count) AS invocation_count,
       sum(error_count) AS error_count,
       approx_percentile(0.5, rollup(duration_percentiles)) AS p50,
       approx_percentile(0.95, rollup(duration_percentiles)) AS p95,
       approx_percentile(0.99, rollup(duration_percentiles)) AS p99
FROM %s
WHERE %s
GROUP BY bucket, command_name
ORDER BY bucket ASC
LIMIT %s OFFSET %s`, table, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: command stats: %w", err)
	}
	defer rows.Close()

	var out []commandStatRow
	for rows.Next() {
		var r commandStatRow
		if err := rows.Scan(&r.Bucket, &r.CommandName, &r.InvocationCount, &r.ErrorCount, &r.P50, &r.P95, &r.P99); err != nil {
			return nil, fmt.Errorf("storage: scan command stats: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatencyPoint is one (bucket, command) percentile-latency observation.
type LatencyPoint struct {
	Bucket          time.Time `json:"bucket"`
	CommandName     string    `json:"command_name"`
	InvocationCount int64     `json:"invocation_count"`
	P50             *float64  `json:"p50_ms,omitempty"`
	P95             *float64  `json:"p95_ms,omitempty"`
	P99             *float64  `json:"p99_ms,omitempty"`
}

func (q *QueryBuilder) LatencyPercentiles(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]LatencyPoint, error) {
	rows, err := q.commandStats(ctx, scope, f, bucket)
	if err != nil {
		return nil, err
	}
	out := make([]LatencyPoint, len(rows))
	for i, r := range rows {
		out[i] = LatencyPoint{Bucket: r.Bucket, CommandName: r.CommandName, InvocationCount: r.InvocationCount, P50: r.P50, P95: r.P95, P99: r.P99}
	}
	return out, nil
}

// ErrorRatePoint is one (bucket, command) error-rate observation.
type ErrorRatePoint struct {
	Bucket          time.Time `json:"bucket"`
	CommandName     string    `json:"command_name"`
	InvocationCount int64     `json:"invocation_count"`
	ErrorCount      int64     `json:"error_count"`
	ErrorRate       float64   `json:"error_rate"` // ErrorCount / InvocationCount; 0 if InvocationCount is 0
}

func (q *QueryBuilder) ErrorRateSeries(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]ErrorRatePoint, error) {
	rows, err := q.commandStats(ctx, scope, f, bucket)
	if err != nil {
		return nil, err
	}
	out := make([]ErrorRatePoint, len(rows))
	for i, r := range rows {
		rate := 0.0
		if r.InvocationCount > 0 {
			rate = float64(r.ErrorCount) / float64(r.InvocationCount)
		}
		out[i] = ErrorRatePoint{Bucket: r.Bucket, CommandName: r.CommandName, InvocationCount: r.InvocationCount, ErrorCount: r.ErrorCount, ErrorRate: rate}
	}
	return out, nil
}

// FrequencyPoint is one (bucket, command) invocation-count observation —
// the command-frequency histogram.
type FrequencyPoint struct {
	Bucket          time.Time `json:"bucket"`
	CommandName     string    `json:"command_name"`
	InvocationCount int64     `json:"invocation_count"`
}

func (q *QueryBuilder) CommandFrequency(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]FrequencyPoint, error) {
	rows, err := q.commandStats(ctx, scope, f, bucket)
	if err != nil {
		return nil, err
	}
	out := make([]FrequencyPoint, len(rows))
	for i, r := range rows {
		out[i] = FrequencyPoint{Bucket: r.Bucket, CommandName: r.CommandName, InvocationCount: r.InvocationCount}
	}
	return out, nil
}

// --- session/cohort activity, off cagg_session_activity_daily ---------

// CohortPoint is one time-bucketed session-count observation, for
// funnel/cohort-style activation dashboards.
type CohortPoint struct {
	Bucket       time.Time `json:"bucket"`
	CLIVersion   *string   `json:"cli_version,omitempty"`
	OS           *string   `json:"os,omitempty"`
	SessionCount int64     `json:"session_count"`
}

func (q *QueryBuilder) SessionCohort(ctx context.Context, scope TenantScope, f Filters) ([]CohortPoint, error) {
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("bucket >= $%d", f.Time.From)
	pb.add("bucket < $%d", f.Time.To)
	pb.addIfNotNilStr("cli_version", f.CLIVersion)
	pb.addIfNotNilStr("os", f.OS)
	pb.addIfNotNilBool("is_ci", f.IsCI)

	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT bucket, cli_version, os, sum(session_count) AS session_count
FROM cagg_session_activity_daily
WHERE %s
GROUP BY bucket, cli_version, os
ORDER BY bucket ASC
LIMIT %s OFFSET %s`, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: session cohort: %w", err)
	}
	defer rows.Close()

	var out []CohortPoint
	for rows.Next() {
		var r CohortPoint
		if err := rows.Scan(&r.Bucket, &r.CLIVersion, &r.OS, &r.SessionCount); err != nil {
			return nil, fmt.Errorf("storage: scan session cohort: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// clampLimit is a last-resort backstop, not the primary page-size
// enforcement — that's internal/metricsapi, using
// MetricsConfig.MaxResultPageSize/DefaultResultPageSize (storage
// deliberately has no config dependency). This just guarantees no query
// built here can ever request an unbounded or negative page.
func clampLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}
