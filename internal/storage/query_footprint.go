package storage

import (
	"context"
	"fmt"
	"sort"
)

// Session footprint rollup (migrations/0010).
const caggSessionFootprintDaily = "cagg_session_footprint_daily"

// BinarySizeStats is the executable size, in bytes, over the sessions that
// reported it. ReportedSessions is the sample size; the figures are omitted
// when it is zero. ReportedInstalls is the approximate number of distinct
// installs behind those sessions (basic tier or above only, as only those
// carry an install id).
type BinarySizeStats struct {
	ReportedSessions int64    `json:"reported_sessions"`
	ReportedInstalls int64    `json:"reported_installs"`
	Median           *float64 `json:"median,omitempty"`
	Min              *int64   `json:"min,omitempty"`
	Max              *int64   `json:"max,omitempty"`
}

// StartupStats is the startup overhead, in milliseconds, over the sessions
// that reported it, with the same sample-size fields as BinarySizeStats.
type StartupStats struct {
	ReportedSessions int64    `json:"reported_sessions"`
	ReportedInstalls int64    `json:"reported_installs"`
	P50              *float64 `json:"p50,omitempty"`
	P95              *float64 `json:"p95,omitempty"`
}

// VersionFootprintPoint is the binary size and startup overhead of one CLI
// version over the query window. SessionCount minus a measure's
// ReportedSessions is the number of sessions that did not report it.
type VersionFootprintPoint struct {
	CLIVersion   *string         `json:"cli_version"`
	SessionCount int64           `json:"session_count"`
	BinarySize   BinarySizeStats `json:"binary_size_bytes"`
	Startup      StartupStats    `json:"startup_duration_ms"`
}

// VersionFootprint returns binary size and startup overhead per CLI version
// over f.Time, off the session footprint rollup. Sketches are merged with
// rollup(), so the percentiles are true percentiles over the window, not
// averages of daily ones. The rollup is fed from cli.session.started logs, so
// history that was never materialized before its raw logs aged out (log
// retention, ~90 days by default) cannot be rebuilt; treat that as the
// reliable horizon.
//
// Results are ordered oldest version first (compareVersionKeys: semver, then
// non-semver strings, then missing), which SQL can't do, so the whole set of
// versions in the window is read and f.Offset/f.Limit applied afterwards;
// that set is as small as the number of releases in the window.
func (q *QueryBuilder) VersionFootprint(ctx context.Context, scope TenantScope, f Filters) ([]VersionFootprintPoint, error) {
	pb := sessionActivityPredicates(scope, f)

	// A sketch over no values has no percentiles, so each is guarded on its
	// reported count rather than trusting approx_percentile on an empty one;
	// an install sketch over no values counts as NULL, hence the coalesce.
	sql := fmt.Sprintf(`
SELECT cli_version,
       sum(session_count) AS session_count,
       sum(size_reported_count) AS size_reported_count,
       coalesce(distinct_count(rollup(size_install_hll)), 0) AS size_installs,
       min(size_min) AS size_min,
       max(size_max) AS size_max,
       CASE WHEN sum(size_reported_count) > 0 THEN approx_percentile(0.5, rollup(size_percentiles)) END AS size_p50,
       sum(startup_reported_count) AS startup_reported_count,
       coalesce(distinct_count(rollup(startup_install_hll)), 0) AS startup_installs,
       CASE WHEN sum(startup_reported_count) > 0 THEN approx_percentile(0.5, rollup(startup_percentiles)) END AS startup_p50,
       CASE WHEN sum(startup_reported_count) > 0 THEN approx_percentile(0.95, rollup(startup_percentiles)) END AS startup_p95
FROM %s
WHERE %s
GROUP BY cli_version`, caggSessionFootprintDaily, pb.where())

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: version footprint: %w", err)
	}
	defer rows.Close()

	var out []VersionFootprintPoint
	for rows.Next() {
		var p VersionFootprintPoint
		if err := rows.Scan(&p.CLIVersion, &p.SessionCount,
			&p.BinarySize.ReportedSessions, &p.BinarySize.ReportedInstalls, &p.BinarySize.Min, &p.BinarySize.Max, &p.BinarySize.Median,
			&p.Startup.ReportedSessions, &p.Startup.ReportedInstalls, &p.Startup.P50, &p.Startup.P95); err != nil {
			return nil, fmt.Errorf("storage: scan version footprint: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(out, func(i, j int) bool {
		return compareVersionKeys(out[i].CLIVersion, out[j].CLIVersion) < 0
	})
	return pageOf(out, f.Offset, clampLimit(f.Limit)), nil
}

// pageOf returns the window of s that starts at offset and holds at most
// limit elements.
func pageOf[T any](s []T, offset, limit int) []T {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(s) {
		return nil
	}
	end := offset + limit
	if end > len(s) || end < offset {
		end = len(s)
	}
	return s[offset:end]
}
