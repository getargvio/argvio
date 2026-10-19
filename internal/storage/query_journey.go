package storage

import (
	"context"
	"fmt"
	"time"
)

// logBucketExpr is rebucket's counterpart for queries that read raw `logs`
// rather than a rollup: any granularity is available, since there is no
// rollup grain to be finer than.
func logBucketExpr(b Bucket) (string, error) {
	if _, ok := bucketRank[b]; !ok {
		return "", fmt.Errorf("storage: unsupported bucket %q", b)
	}
	return fmt.Sprintf("date_trunc('%s', time, 'UTC')", b), nil
}

// --- error-type distribution, off raw logs ------------------------------

// ErrorTypePoint is one (bucket, command, error type) error count. ErrorType
// is the coarse vendor-assigned category (network, timeout, canceled, ...),
// nil when the event carried none.
type ErrorTypePoint struct {
	Bucket      time.Time `json:"bucket"`
	CommandName *string   `json:"command_name"`
	ErrorType   *string   `json:"error_type"`
	ErrorCount  int64     `json:"error_count"`
}

// ErrorTypes counts cli.error.raised events by cli.error.type. The SDKs
// only emit that event at the full consent tier or above, so this
// undercounts relative to ErrorRateSeries, which sees every failed command.
// Reads raw logs (no rollup), so it is bounded by log retention.
func (q *QueryBuilder) ErrorTypes(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]ErrorTypePoint, error) {
	bucketExpr, err := logBucketExpr(bucket)
	if err != nil {
		return nil, err
	}

	pb := errorEventPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, command_name, log_attributes ->> 'cli.error.type' AS error_type, count(*) AS error_count
FROM logs
WHERE %s
GROUP BY 1, 2, 3
ORDER BY 1 ASC, 2 ASC NULLS LAST, 3 ASC NULLS LAST
LIMIT %s OFFSET %s`, bucketExpr, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: error types: %w", err)
	}
	defer rows.Close()

	var out []ErrorTypePoint
	for rows.Next() {
		var r ErrorTypePoint
		if err := rows.Scan(&r.Bucket, &r.CommandName, &r.ErrorType, &r.ErrorCount); err != nil {
			return nil, fmt.Errorf("storage: scan error types: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ErrorReasonPoint is one (bucket, command, error type, error reason) error
// count. ErrorReason is the vendor's finer cause within ErrorType, nil when
// the event carried none (an SDK that doesn't send it, or a value the
// allowlist pattern dropped), and "other" for values past the tenant's
// reason cap.
type ErrorReasonPoint struct {
	Bucket      time.Time `json:"bucket"`
	CommandName *string   `json:"command_name"`
	ErrorType   *string   `json:"error_type"`
	ErrorReason *string   `json:"error_reason"`
	ErrorCount  int64     `json:"error_count"`
}

// ErrorReasons is ErrorTypes broken down further by cli.error.reason. The
// reason is basic tier, but it is read from the same cli.error.raised events,
// so the same undercount and log retention bound apply.
func (q *QueryBuilder) ErrorReasons(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]ErrorReasonPoint, error) {
	bucketExpr, err := logBucketExpr(bucket)
	if err != nil {
		return nil, err
	}
	pb := errorEventPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, command_name,
       log_attributes ->> 'cli.error.type' AS error_type,
       log_attributes ->> 'cli.error.reason' AS error_reason,
       count(*) AS error_count
FROM logs
WHERE %s
GROUP BY 1, 2, 3, 4
ORDER BY 1 ASC, 2 ASC NULLS LAST, 3 ASC NULLS LAST, 4 ASC NULLS LAST
LIMIT %s OFFSET %s`, bucketExpr, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: error reasons: %w", err)
	}
	defer rows.Close()

	var out []ErrorReasonPoint
	for rows.Next() {
		var r ErrorReasonPoint
		if err := rows.Scan(&r.Bucket, &r.CommandName, &r.ErrorType, &r.ErrorReason, &r.ErrorCount); err != nil {
			return nil, fmt.Errorf("storage: scan error reasons: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// errorEventPredicates selects the tenant's cli.error.raised events in f.Time
// with the command, version, os, arch and is_ci filters.
func errorEventPredicates(scope TenantScope, f Filters) *predicateBuilder {
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("event_name = $%d", "cli.error.raised")
	pb.add("time >= $%d", f.Time.From)
	pb.add("time < $%d", f.Time.To)
	pb.addIfAnyStr("command_name", f.CommandNames)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	return pb
}

// --- friction signals: help views and interrupted runs ------------------

// FrictionPoint is one (bucket, command) observation of signals that a
// command is hard to use. HelpViews can be non-zero while InvocationCount
// is zero: asking for help doesn't run the command.
type FrictionPoint struct {
	Bucket           time.Time `json:"bucket"`
	CommandName      *string   `json:"command_name"`
	InvocationCount  int64     `json:"invocation_count"`
	HelpViews        int64     `json:"help_views"`
	InterruptedCount int64     `json:"interrupted_count"`
}

// interruptedExitCode is the conventional shell exit status for SIGINT
// (128 + 2), i.e. Ctrl+C. Nothing in the SDKs forces a CLI to exit with it
// when interrupted, so InterruptedCount is a lower bound.
const interruptedExitCode = 130

// Friction joins invocation and interrupted-run counts (from the hourly
// exit-code rollup, so every consent tier is counted) with cli.help.viewed
// events (raw logs, so bounded by log retention). The exit_code and attr
// filters don't apply.
func (q *QueryBuilder) Friction(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]FrictionPoint, error) {
	rollupBucket, err := rebucket(bucket, BucketHour)
	if err != nil {
		return nil, err
	}
	logBucket, err := logBucketExpr(bucket)
	if err != nil {
		return nil, err
	}

	// One argument list shared by both CTEs: every column the filters touch
	// has the same name in the rollup and in logs.
	pb := newScopedPredicates(scope, "tenant_id")
	pb.addIfAnyStr("command_name", f.CommandNames)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	fromPh := pb.nextPlaceholder(f.Time.From)
	toPh := pb.nextPlaceholder(f.Time.To)
	helpEventPh := pb.nextPlaceholder("cli.help.viewed")
	interruptedPh := pb.nextPlaceholder(int32(interruptedExitCode))
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
WITH invoked AS (
	SELECT %[1]s AS bucket, command_name,
	       sum(invocation_count) AS invocation_count,
	       coalesce(sum(invocation_count) FILTER (WHERE exit_code = %[6]s), 0) AS interrupted_count
	FROM cagg_exit_codes_hourly
	WHERE %[3]s AND bucket >= %[4]s AND bucket < %[5]s
	GROUP BY 1, 2
),
helped AS (
	SELECT %[2]s AS bucket, command_name, count(*) AS help_views
	FROM logs
	WHERE %[3]s AND event_name = %[7]s AND time >= %[4]s AND time < %[5]s
	GROUP BY 1, 2
)
SELECT coalesce(i.bucket, h.bucket) AS bucket,
       coalesce(i.command_name, h.command_name) AS command_name,
       coalesce(i.invocation_count, 0) AS invocation_count,
       coalesce(h.help_views, 0) AS help_views,
       coalesce(i.interrupted_count, 0) AS interrupted_count
FROM invoked i
FULL OUTER JOIN helped h ON i.bucket = h.bucket AND i.command_name IS NOT DISTINCT FROM h.command_name
ORDER BY 1 ASC, 2 ASC NULLS LAST
LIMIT %[8]s OFFSET %[9]s`,
		rollupBucket, logBucket, pb.where(), fromPh, toPh, interruptedPh, helpEventPh, limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: friction: %w", err)
	}
	defer rows.Close()

	var out []FrictionPoint
	for rows.Next() {
		var r FrictionPoint
		if err := rows.Scan(&r.Bucket, &r.CommandName, &r.InvocationCount, &r.HelpViews, &r.InterruptedCount); err != nil {
			return nil, fmt.Errorf("storage: scan friction: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
