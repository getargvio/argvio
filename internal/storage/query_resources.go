package storage

import (
	"context"
	"fmt"
	"time"
)

// Resource-usage rollups (migrations/0009), same hourly/daily layering as the
// command-stats ones.
const (
	caggCommandResourcesHourly = "cagg_command_resources_hourly"
	caggCommandResourcesDaily  = "cagg_command_resources_daily"
)

// UsageStats is the distribution of one resource measure over the runs in a
// bucket that reported it. ReportedCount is the sample size behind the
// percentiles; the percentiles are omitted when it is zero.
type UsageStats struct {
	ReportedCount int64    `json:"reported_count"`
	P50           *float64 `json:"p50,omitempty"`
	P95           *float64 `json:"p95,omitempty"`
	P99           *float64 `json:"p99,omitempty"`
}

// ResourceUsagePoint is one (bucket, command, version) observation of peak
// memory and CPU time. InvocationCount minus a measure's ReportedCount is
// the number of runs that did not report it (SDKs on platforms that cannot
// measure omit the attribute).
type ResourceUsagePoint struct {
	Bucket          time.Time  `json:"bucket"`
	CommandName     string     `json:"command_name"`
	CLIVersion      *string    `json:"cli_version"`
	InvocationCount int64      `json:"invocation_count"`
	PeakRSSBytes    UsageStats `json:"peak_rss_bytes"`
	CPUTimeMs       UsageStats `json:"cpu_time_ms"`
}

// ResourceUsage returns peak-memory and CPU-time percentiles per command and
// CLI version, off the resource-usage rollups. Percentile sketches are merged
// with rollup() so day/week/month buckets are true percentiles over the
// period, not averages of hourly ones.
func (q *QueryBuilder) ResourceUsage(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]ResourceUsagePoint, error) {
	table, grain := caggCommandResourcesHourly, BucketHour
	if bucket != BucketHour {
		table, grain = caggCommandResourcesDaily, BucketDay
	}
	bucketExpr, err := rebucket(bucket, grain)
	if err != nil {
		return nil, err
	}

	pb := commandStatsPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	// A sketch built from zero values has no percentiles, so each is guarded
	// on its reported count rather than trusting approx_percentile on an
	// empty sketch.
	sql := fmt.Sprintf(`
SELECT %s AS bucket, command_name, cli_version,
       sum(invocation_count) AS invocation_count,
       sum(rss_reported_count) AS rss_reported_count,
       CASE WHEN sum(rss_reported_count) > 0 THEN approx_percentile(0.5, rollup(rss_percentiles)) END AS rss_p50,
       CASE WHEN sum(rss_reported_count) > 0 THEN approx_percentile(0.95, rollup(rss_percentiles)) END AS rss_p95,
       CASE WHEN sum(rss_reported_count) > 0 THEN approx_percentile(0.99, rollup(rss_percentiles)) END AS rss_p99,
       sum(cpu_reported_count) AS cpu_reported_count,
       CASE WHEN sum(cpu_reported_count) > 0 THEN approx_percentile(0.5, rollup(cpu_percentiles)) END AS cpu_p50,
       CASE WHEN sum(cpu_reported_count) > 0 THEN approx_percentile(0.95, rollup(cpu_percentiles)) END AS cpu_p95,
       CASE WHEN sum(cpu_reported_count) > 0 THEN approx_percentile(0.99, rollup(cpu_percentiles)) END AS cpu_p99
FROM %s
WHERE %s
GROUP BY 1, 2, 3
ORDER BY 1 ASC, 2 ASC, 3 ASC NULLS LAST
LIMIT %s OFFSET %s`, bucketExpr, table, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: resource usage: %w", err)
	}
	defer rows.Close()

	var out []ResourceUsagePoint
	for rows.Next() {
		var p ResourceUsagePoint
		if err := rows.Scan(&p.Bucket, &p.CommandName, &p.CLIVersion, &p.InvocationCount,
			&p.PeakRSSBytes.ReportedCount, &p.PeakRSSBytes.P50, &p.PeakRSSBytes.P95, &p.PeakRSSBytes.P99,
			&p.CPUTimeMs.ReportedCount, &p.CPUTimeMs.P50, &p.CPUTimeMs.P95, &p.CPUTimeMs.P99); err != nil {
			return nil, fmt.Errorf("storage: scan resource usage: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
