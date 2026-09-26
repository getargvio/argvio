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
//
// The slice-valued dimensions have OR semantics within a field (a row
// matches if its value is any of the listed ones) and AND semantics across
// fields, matching how Attrs already ANDs distinct keys.
type Filters struct {
	Time TimeRange

	CLIVersions  []string
	OSes         []string
	Arches       []string
	CommandNames []string
	ExitCode     *int32
	IsCI         *bool

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

func (pb *predicateBuilder) addIfAnyStr(col string, vs []string) {
	if len(vs) > 0 {
		pb.add(col+" = ANY($%d)", vs)
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
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfAnyStr("command_name", f.CommandNames)
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

// Bucket is the time-bucket granularity of an aggregate query. hour and day
// read the matching materialized rollup directly
// (migrations/0003_continuous_aggregates.up.sql); week and month are
// re-bucketed at query time from the daily rollup — counts sum and
// percentile sketches combine via rollup(), so neither needs its own
// continuous aggregate. Anything finer than hour would require scanning raw
// traces, which ListTraces is for.
type Bucket string

const (
	BucketHour  Bucket = "hour"
	BucketDay   Bucket = "day"
	BucketWeek  Bucket = "week"
	BucketMonth Bucket = "month"
)

// Continuous-aggregate table names referenced from multiple query paths.
const (
	caggCommandStatsDaily    = "cagg_command_stats_daily"
	caggSessionActivityDaily = "cagg_session_activity_daily"
)

// bucketRank orders granularities finest-first, so rebucket can tell
// whether a requested granularity is recoverable from a given rollup.
var bucketRank = map[Bucket]int{BucketHour: 0, BucketDay: 1, BucketWeek: 2, BucketMonth: 3}

// rebucket returns the SQL expression that groups a rollup whose `bucket`
// column has granularity grain into granularity b. The expression is one of
// a fixed set of literals (b is validated against bucketRank first), never
// built from request input. date_trunc's explicit 'UTC' zone keeps
// day/week/month boundaries independent of the connection's TimeZone
// setting; weeks start on Monday (ISO 8601).
func rebucket(b, grain Bucket) (string, error) {
	rank, ok := bucketRank[b]
	if !ok {
		return "", fmt.Errorf("storage: unsupported bucket %q", b)
	}
	if rank < bucketRank[grain] {
		return "", fmt.Errorf("storage: bucket %q is finer than this rollup's %q granularity", b, grain)
	}
	if b == grain {
		return "bucket", nil
	}
	return fmt.Sprintf("date_trunc('%s', bucket, 'UTC')", b), nil
}

// commandStatsSource picks the command-stats rollup backing b — hourly for
// bucket=hour, daily (re-bucketed as needed) for everything coarser.
func commandStatsSource(b Bucket) (table, bucketExpr string, err error) {
	if b == BucketHour {
		return "cagg_command_stats_hourly", "bucket", nil
	}
	bucketExpr, err = rebucket(b, BucketDay)
	return caggCommandStatsDaily, bucketExpr, err
}

// commandStatsPredicates applies the filters every command-stats-backed
// query shares (the rollups carry no exit_code or attribute columns).
func commandStatsPredicates(scope TenantScope, f Filters) *predicateBuilder {
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("bucket >= $%d", f.Time.From)
	pb.add("bucket < $%d", f.Time.To)
	pb.addIfAnyStr("command_name", f.CommandNames)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	return pb
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
	table, bucketExpr, err := commandStatsSource(bucket)
	if err != nil {
		return nil, err
	}

	pb := commandStatsPredicates(scope, f)
	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	// GROUP BY/ORDER BY are positional: a bare `bucket` in GROUP BY would
	// resolve to the input column, not the re-bucketed output alias.
	sql := fmt.Sprintf(`
SELECT %s AS bucket, command_name,
       sum(invocation_count) AS invocation_count,
       sum(error_count) AS error_count,
       approx_percentile(0.5, rollup(duration_percentiles)) AS p50,
       approx_percentile(0.95, rollup(duration_percentiles)) AS p95,
       approx_percentile(0.99, rollup(duration_percentiles)) AS p99
FROM %s
WHERE %s
GROUP BY 1, 2
ORDER BY 1 ASC, 2 ASC
LIMIT %s OFFSET %s`, bucketExpr, table, pb.where(), limitPh, offsetPh)

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

// CISplitPoint is one (bucket, is_ci) invocation-count observation. IsCI is
// nil for invocations whose client didn't report cli.is_ci.
type CISplitPoint struct {
	Bucket          time.Time `json:"bucket"`
	IsCI            *bool     `json:"is_ci"`
	InvocationCount int64     `json:"invocation_count"`
}

// CISplit groups command invocations by is_ci in one query, off the same
// rollups as the command-stats queries.
func (q *QueryBuilder) CISplit(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]CISplitPoint, error) {
	table, bucketExpr, err := commandStatsSource(bucket)
	if err != nil {
		return nil, err
	}

	pb := commandStatsPredicates(scope, f)
	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, is_ci, sum(invocation_count) AS invocation_count
FROM %s
WHERE %s
GROUP BY 1, 2
ORDER BY 1 ASC, 2 ASC NULLS LAST
LIMIT %s OFFSET %s`, bucketExpr, table, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: ci split: %w", err)
	}
	defer rows.Close()

	var out []CISplitPoint
	for rows.Next() {
		var r CISplitPoint
		if err := rows.Scan(&r.Bucket, &r.IsCI, &r.InvocationCount); err != nil {
			return nil, fmt.Errorf("storage: scan ci split: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- exit-code distribution, off cagg_exit_codes_hourly ---------------

// ExitCodePoint is one (bucket, command, exit_code) invocation-count
// observation. ExitCode is nil for invocations whose client didn't report
// cli.command.exit_code.
type ExitCodePoint struct {
	Bucket          time.Time `json:"bucket"`
	CommandName     string    `json:"command_name"`
	ExitCode        *int32    `json:"exit_code"`
	InvocationCount int64     `json:"invocation_count"`
}

// ExitCodeDistribution backs the error-type breakdown: the command-stats
// rollups only carry an errored/not-errored count, so exit codes get their
// own hourly rollup (migrations/0005), re-bucketed for day/week/month.
func (q *QueryBuilder) ExitCodeDistribution(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]ExitCodePoint, error) {
	bucketExpr, err := rebucket(bucket, BucketHour)
	if err != nil {
		return nil, err
	}

	pb := commandStatsPredicates(scope, f)
	pb.addIfNotNilI32("exit_code", f.ExitCode)
	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, command_name, exit_code, sum(invocation_count) AS invocation_count
FROM cagg_exit_codes_hourly
WHERE %s
GROUP BY 1, 2, 3
ORDER BY 1 ASC, 2 ASC, 3 ASC NULLS LAST
LIMIT %s OFFSET %s`, bucketExpr, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: exit code distribution: %w", err)
	}
	defer rows.Close()

	var out []ExitCodePoint
	for rows.Next() {
		var r ExitCodePoint
		if err := rows.Scan(&r.Bucket, &r.CommandName, &r.ExitCode, &r.InvocationCount); err != nil {
			return nil, fmt.Errorf("storage: scan exit code distribution: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- session/cohort activity, off cagg_session_activity_daily ---------

func sessionActivityPredicates(scope TenantScope, f Filters) *predicateBuilder {
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("bucket >= $%d", f.Time.From)
	pb.add("bucket < $%d", f.Time.To)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	return pb
}

// CohortPoint is one time-bucketed session-count observation, for
// funnel/cohort-style activation dashboards.
type CohortPoint struct {
	Bucket       time.Time `json:"bucket"`
	CLIVersion   *string   `json:"cli_version,omitempty"`
	OS           *string   `json:"os,omitempty"`
	Arch         *string   `json:"arch,omitempty"`
	SessionCount int64     `json:"session_count"`
}

// SessionCohort reads the daily session rollup, so bucket must be day or
// coarser.
func (q *QueryBuilder) SessionCohort(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]CohortPoint, error) {
	bucketExpr, err := rebucket(bucket, BucketDay)
	if err != nil {
		return nil, err
	}

	pb := sessionActivityPredicates(scope, f)
	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, cli_version, os, arch, sum(session_count) AS session_count
FROM %s
WHERE %s
GROUP BY 1, 2, 3, 4
ORDER BY 1 ASC
LIMIT %s OFFSET %s`, bucketExpr, caggSessionActivityDaily, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: session cohort: %w", err)
	}
	defer rows.Close()

	var out []CohortPoint
	for rows.Next() {
		var r CohortPoint
		if err := rows.Scan(&r.Bucket, &r.CLIVersion, &r.OS, &r.Arch, &r.SessionCount); err != nil {
			return nil, fmt.Errorf("storage: scan session cohort: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveInstallsPoint is the approximate number of distinct installs
// (cli.install_id) that started at least one session in a bucket — DAU,
// WAU or MAU depending on the bucket.
type ActiveInstallsPoint struct {
	Bucket         time.Time `json:"bucket"`
	ActiveInstalls int64     `json:"active_installs"`
}

// ActiveInstalls unions the per-day HyperLogLog sketches in
// cagg_session_activity_daily, so a distinct count over a week/month is
// correct (an install active on several days counts once) rather than a sum
// of daily counts. Approximate (~1% standard error), and only covers
// sessions recorded at basic consent tier or above — anonymous-tier records
// never carry cli.install_id.
func (q *QueryBuilder) ActiveInstalls(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]ActiveInstallsPoint, error) {
	bucketExpr, err := rebucket(bucket, BucketDay)
	if err != nil {
		return nil, err
	}

	pb := sessionActivityPredicates(scope, f)
	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, distinct_count(rollup(install_hll)) AS active_installs
FROM %s
WHERE %s
GROUP BY 1
ORDER BY 1 ASC
LIMIT %s OFFSET %s`, bucketExpr, caggSessionActivityDaily, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: active installs: %w", err)
	}
	defer rows.Close()

	var out []ActiveInstallsPoint
	for rows.Next() {
		var r ActiveInstallsPoint
		if err := rows.Scan(&r.Bucket, &r.ActiveInstalls); err != nil {
			return nil, fmt.Errorf("storage: scan active installs: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- retention by cohort, off raw logs --------------------------------

// RetentionPoint is one cell of a cohort retention grid: of the
// CohortSize installs first seen in the Cohort bucket, RetainedInstalls
// started a session Period buckets later (Period 0 is the cohort bucket
// itself, so RetainedInstalls == CohortSize there).
type RetentionPoint struct {
	Cohort           time.Time `json:"cohort"`
	Period           int       `json:"period"`
	CohortSize       int64     `json:"cohort_size"`
	RetainedInstalls int64     `json:"retained_installs"`
}

// retentionPeriodExpr returns how many cohortBucket-sized steps separate
// timestamps a and b (both already date_trunc'd to that bucket in UTC).
// Fixed literals only, like rebucket.
func retentionPeriodExpr(b Bucket, a, c string) (string, error) {
	switch b {
	case BucketDay:
		return fmt.Sprintf("(extract(epoch FROM %s - %s) / 86400)::int", a, c), nil
	case BucketWeek:
		return fmt.Sprintf("(extract(epoch FROM %s - %s) / 604800)::int", a, c), nil
	case BucketMonth:
		return fmt.Sprintf("((extract(year FROM %[1]s AT TIME ZONE 'UTC') - extract(year FROM %[2]s AT TIME ZONE 'UTC')) * 12"+
			" + extract(month FROM %[1]s AT TIME ZONE 'UTC') - extract(month FROM %[2]s AT TIME ZONE 'UTC'))::int", a, c), nil
	default:
		return "", fmt.Errorf("storage: unsupported retention cohort bucket %q (want day, week or month)", b)
	}
}

// Retention computes an install-level retention grid for cohorts first
// seen within f.Time. This is the one aggregate that reads raw logs rather
// than a continuous aggregate: first-seen is a per-install minimum over all
// history, which no time-bucketed rollup can answer. "First seen" is
// therefore bounded by raw log retention (installs older than that look
// new), and — like ActiveInstalls — only covers basic-tier-or-above
// sessions carrying cli.install_id. Dimension filters restrict which
// sessions count, so filtering by cli_version gives "first seen on this
// version" cohorts.
func (q *QueryBuilder) Retention(ctx context.Context, scope TenantScope, f Filters, cohortBucket Bucket) ([]RetentionPoint, error) {
	periodExpr, err := retentionPeriodExpr(cohortBucket, "s.period_start", "c.cohort")
	if err != nil {
		return nil, err
	}

	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("event_name = $%d", "cli.session.started")
	pb.add("time < $%d", f.Time.To)
	pb.clauses = append(pb.clauses, "log_attributes ? 'cli.install_id'")
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	fromPh := pb.nextPlaceholder(f.Time.From)
	limit := clampLimit(f.Limit)
	limitPh := pb.nextPlaceholder(limit)
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
WITH sessions AS (
	SELECT DISTINCT log_attributes ->> 'cli.install_id' AS install_id,
	       date_trunc('%[1]s', time, 'UTC') AS period_start
	FROM logs
	WHERE %[2]s
),
cohorts AS (
	SELECT install_id, min(period_start) AS cohort
	FROM sessions
	GROUP BY install_id
),
retained AS (
	SELECT c.cohort, %[3]s AS period, count(*) AS retained_installs
	FROM sessions s
	JOIN cohorts c USING (install_id)
	WHERE c.cohort >= date_trunc('%[1]s', %[4]s::timestamptz, 'UTC')
	GROUP BY 1, 2
)
SELECT cohort, period,
       first_value(retained_installs) OVER (PARTITION BY cohort ORDER BY period) AS cohort_size,
       retained_installs
FROM retained
ORDER BY cohort ASC, period ASC
LIMIT %[5]s OFFSET %[6]s`, cohortBucket, pb.where(), periodExpr, fromPh, limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: retention: %w", err)
	}
	defer rows.Close()

	var out []RetentionPoint
	for rows.Next() {
		var r RetentionPoint
		if err := rows.Scan(&r.Cohort, &r.Period, &r.CohortSize, &r.RetainedInstalls); err != nil {
			return nil, fmt.Errorf("storage: scan retention: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- distinct dimension values ----------------------------------------

// Dimensions lists the distinct values a tenant has data for, per filter
// dimension — the source for dashboard filter option lists.
type Dimensions struct {
	CLIVersions  []string `json:"cli_version"`
	OSes         []string `json:"os"`
	Arches       []string `json:"arch"`
	CommandNames []string `json:"command_name"`
}

// dimensionSources maps each Dimensions column to the daily rollups that
// carry it. Both tables and columns are fixed identifiers, never input.
var dimensionSources = []struct {
	column string
	tables []string
}{
	{"cli_version", []string{caggCommandStatsDaily, caggSessionActivityDaily}},
	{"os", []string{caggCommandStatsDaily, caggSessionActivityDaily}},
	{"arch", []string{caggCommandStatsDaily, caggSessionActivityDaily}},
	{"command_name", []string{caggCommandStatsDaily}},
}

// ListDimensions reads the daily rollups (the longest-retained data) with
// no time-range filter, so option lists cover everything the tenant has
// data for rather than whatever range a dashboard happened to query. Each
// list is sorted and capped at limit values.
func (q *QueryBuilder) ListDimensions(ctx context.Context, scope TenantScope, limit int) (Dimensions, error) {
	var out Dimensions
	targets := []*[]string{&out.CLIVersions, &out.OSes, &out.Arches, &out.CommandNames}

	for i, src := range dimensionSources {
		pb := newScopedPredicates(scope, "tenant_id")
		pb.clauses = append(pb.clauses, src.column+" IS NOT NULL")
		selects := make([]string, len(src.tables))
		for j, table := range src.tables {
			selects[j] = fmt.Sprintf("SELECT DISTINCT %s AS value FROM %s WHERE %s", src.column, table, pb.where())
		}
		limitPh := pb.nextPlaceholder(clampLimit(limit))
		// DISTINCT dedupes within a rollup, UNION (not UNION ALL) across them.
		sql := fmt.Sprintf("%s ORDER BY value ASC LIMIT %s", strings.Join(selects, " UNION "), limitPh)

		values := []string{}
		rows, err := q.pool.Query(ctx, sql, pb.args...)
		if err != nil {
			return out, fmt.Errorf("storage: list %s values: %w", src.column, err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return out, fmt.Errorf("storage: scan %s value: %w", src.column, err)
			}
			values = append(values, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, fmt.Errorf("storage: list %s values: %w", src.column, err)
		}
		*targets[i] = values
	}
	return out, nil
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
