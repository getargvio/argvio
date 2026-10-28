package storage

import (
	"context"
	"fmt"
	"time"
)

// Cold vs. warm command latency, derived server-side from raw
// cli.command.completed logs joined to the install state. See
// docs/journey-telemetry-design.md "Cold vs. warm latency".

const attrCommandDurationMs = "cli.command.duration_ms"

// RunLatency is the latency of one class of runs (cold or warm) of a command
// in a bucket. RunCount is the sample size behind the percentiles: the runs
// that reported a duration. The percentiles are absent when it is 0.
type RunLatency struct {
	RunCount int64    `json:"run_count"`
	P50      *float64 `json:"p50_ms,omitempty"`
	P95      *float64 `json:"p95_ms,omitempty"`
	P99      *float64 `json:"p99_ms,omitempty"`
}

// ColdWarmLatencyPoint is one (bucket, command) observation with cold and warm
// runs reported separately.
type ColdWarmLatencyPoint struct {
	Bucket      time.Time  `json:"bucket"`
	CommandName string     `json:"command_name"`
	Cold        RunLatency `json:"cold"`
	Warm        RunLatency `json:"warm"`
}

// ColdWarmLatency reports p50/p95/p99 command duration, and the number of runs
// behind them, for cold and warm runs separately. A run is a
// cli.command.completed carrying a cli.install_id, a command name and a
// cli.command.duration_ms. It is cold when it is
//
//   - the install's first completed command ever (installs.first_command_at),
//     or
//   - the first completed command at or after a version change recorded for
//     the install (install_version_changes.at),
//
// whatever the command and whichever filters are set: the filters pick the
// runs reported, not which runs are cold. Every other run is warm. At most one
// run per install per anchor is cold, so cold groups are small; read RunCount
// before trusting a cold percentile.
//
// The first-ever anchor needs the run itself to still be in the logs, so a
// first run that has aged out of raw logs is not replaced by a later one. The
// durations are read from raw logs, so results are bounded by log retention.
// The command, cli_version, os, arch and is_ci filters apply to the run;
// exit_code and attr filters don't. f.Limit and f.Offset page the
// (bucket, command) rows.
func (q *QueryBuilder) ColdWarmLatency(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]ColdWarmLatencyPoint, error) {
	bucketExpr, err := bucketOf(bucket, "time")
	if err != nil {
		return nil, err
	}

	pb := sessionEventPredicates(scope, f)
	pb.clauses = append(pb.clauses, "command_name IS NOT NULL")
	pb.clauses = append(pb.clauses, "jsonb_typeof(log_attributes -> '"+attrCommandDurationMs+"') = 'number'")
	toPh := pb.nextPlaceholder(f.Time.To)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)
	// The cold-run lookup reads installs, install_version_changes and logs
	// outside the predicate builder's WHERE, so it is scoped on its own: the
	// builder's first argument is always the tenant id.
	const tenantPh = "$1"
	eventPh := pb.nextPlaceholder(eventCommandCompleted)

	sql := fmt.Sprintf(`
WITH anchors AS (
	SELECT install_id, first_command_at AS at, first_command_at + interval '1 microsecond' AS until
	FROM installs
	WHERE tenant_id = %[1]s AND first_command_at IS NOT NULL AND first_command_at < %[2]s
	UNION ALL
	SELECT install_id, at, %[2]s AS until
	FROM install_version_changes
	WHERE tenant_id = %[1]s AND at < %[2]s
),
cold AS (
	SELECT r.id AS cold_id, r.time AS cold_time
	FROM anchors a
	CROSS JOIN LATERAL (
		SELECT l.id, l.time
		FROM logs l
		WHERE l.tenant_id = %[1]s AND l.event_name = %[3]s
		  AND l.log_attributes ->> 'cli.install_id' = a.install_id
		  AND l.time >= a.at AND l.time < a.until
		ORDER BY l.time, l.id
		LIMIT 1
	) r
	GROUP BY r.id, r.time
),
runs AS (
	SELECT %[4]s AS bucket, command_name,
	       (log_attributes ->> '%[5]s')::double precision AS duration_ms,
	       cold_id IS NOT NULL AS is_cold
	FROM logs
	LEFT JOIN cold ON cold_id = logs.id AND cold_time = logs.time
	WHERE %[6]s
)
SELECT bucket, command_name,
       count(*) FILTER (WHERE is_cold) AS cold_count,
       approx_percentile(0.5, percentile_agg(duration_ms) FILTER (WHERE is_cold)) AS cold_p50,
       approx_percentile(0.95, percentile_agg(duration_ms) FILTER (WHERE is_cold)) AS cold_p95,
       approx_percentile(0.99, percentile_agg(duration_ms) FILTER (WHERE is_cold)) AS cold_p99,
       count(*) FILTER (WHERE NOT is_cold) AS warm_count,
       approx_percentile(0.5, percentile_agg(duration_ms) FILTER (WHERE NOT is_cold)) AS warm_p50,
       approx_percentile(0.95, percentile_agg(duration_ms) FILTER (WHERE NOT is_cold)) AS warm_p95,
       approx_percentile(0.99, percentile_agg(duration_ms) FILTER (WHERE NOT is_cold)) AS warm_p99
FROM runs
WHERE duration_ms >= 0
GROUP BY 1, 2
ORDER BY 1 ASC, 2 ASC
LIMIT %[7]s OFFSET %[8]s`, tenantPh, toPh, eventPh, bucketExpr, attrCommandDurationMs, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: cold/warm latency: %w", err)
	}
	defer rows.Close()

	out := []ColdWarmLatencyPoint{}
	for rows.Next() {
		var p ColdWarmLatencyPoint
		if err := rows.Scan(&p.Bucket, &p.CommandName,
			&p.Cold.RunCount, &p.Cold.P50, &p.Cold.P95, &p.Cold.P99,
			&p.Warm.RunCount, &p.Warm.P50, &p.Warm.P95, &p.Warm.P99); err != nil {
			return nil, fmt.Errorf("storage: scan cold/warm latency: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
