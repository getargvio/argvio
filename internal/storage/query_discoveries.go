package storage

import (
	"context"
	"fmt"
)

const secondsPerDay = 24 * 60 * 60

// DiscoveryPoint describes how quickly the install cohort found one feature
// or flag. Installs counts cohort installs that have ever used it and
// Within*Days those that did so within that long of first being seen, so
// Within7Days/CohortSize is the share of the cohort that discovered it in a
// week. MedianSeconds is over discoverers only, like ActivationPoint's
// percentiles: it says how fast the ones who found it were, not how long the
// rest will take.
type DiscoveryPoint struct {
	Kind          string  `json:"kind"` // feature | flag
	Name          string  `json:"name"`
	CohortSize    int64   `json:"cohort_size"`
	Installs      int64   `json:"installs"`
	Within1Day    int64   `json:"discovered_within_1d"`
	Within7Days   int64   `json:"discovered_within_7d"`
	Within30Days  int64   `json:"discovered_within_30d"`
	MedianSeconds float64 `json:"median_seconds"`
}

// FeatureDiscovery measures first use minus first_seen for the installs first
// seen in f.Time (the same cohort as InstallFunnel), per feature and flag,
// from install_discoveries. kind restricts the result to DiscoveryFeature or
// DiscoveryFlag; empty returns both. Like the funnel, a discovery counts
// whenever it happened, even after the window, so recent cohorts are still
// maturing, and a negative gap (first use stamped before first_seen) counts
// as zero.
//
// Names are vendor-supplied strings, so the result is the top f.Limit by
// installs (ties by name) within each kind, skipping the first f.Offset.
// Discoveries are kept as long as their install is (see sweepInstalls), so
// there's no log retention bound, but an install whose first use predates the
// table (before 0008, beyond the backfill window) looks like a later one.
// Install filters apply as for InstallFunnel; command and is_ci don't.
func (q *QueryBuilder) FeatureDiscovery(ctx context.Context, scope TenantScope, f Filters, kind string) ([]DiscoveryPoint, error) {
	if kind != "" && kind != DiscoveryFeature && kind != DiscoveryFlag {
		return nil, fmt.Errorf("storage: unsupported discovery kind %q", kind)
	}
	pb := installCohortPredicates(scope, f)
	kindClause := ""
	if kind != "" {
		kindClause = "AND d.kind = " + pb.nextPlaceholder(kind)
	}
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	// The join goes through the cohort, which carries the tenant predicate,
	// so discoveries can only be read for the scoped tenant.
	sql := fmt.Sprintf(`
WITH cohort AS (
	SELECT tenant_id, install_id, first_seen FROM installs WHERE %[1]s
),
found AS (
	SELECT d.kind, d.name,
	       greatest(extract(epoch FROM d.first_at - c.first_seen), 0)::float8 AS secs
	FROM cohort c
	JOIN install_discoveries d ON d.tenant_id = c.tenant_id AND d.install_id = c.install_id
	WHERE true %[2]s
),
agg AS (
	SELECT kind, name, count(*) AS installs,
	       count(*) FILTER (WHERE secs <= %[3]d) AS within_1d,
	       count(*) FILTER (WHERE secs <= %[4]d) AS within_7d,
	       count(*) FILTER (WHERE secs <= %[5]d) AS within_30d,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY secs) AS median_seconds
	FROM found
	GROUP BY kind, name
),
ranked AS (
	SELECT *, row_number() OVER (PARTITION BY kind ORDER BY installs DESC, name ASC) AS rn
	FROM agg
)
SELECT kind, name, (SELECT count(*) FROM cohort) AS cohort_size, installs,
       within_1d, within_7d, within_30d, median_seconds
FROM ranked
WHERE rn > %[7]s::bigint AND rn <= %[7]s::bigint + %[6]s::bigint
ORDER BY kind ASC, rn ASC`,
		pb.where(), kindClause, secondsPerDay, 7*secondsPerDay, 30*secondsPerDay, limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: feature discovery: %w", err)
	}
	defer rows.Close()

	var out []DiscoveryPoint
	for rows.Next() {
		var r DiscoveryPoint
		if err := rows.Scan(&r.Kind, &r.Name, &r.CohortSize, &r.Installs,
			&r.Within1Day, &r.Within7Days, &r.Within30Days, &r.MedianSeconds); err != nil {
			return nil, fmt.Errorf("storage: scan feature discovery: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
