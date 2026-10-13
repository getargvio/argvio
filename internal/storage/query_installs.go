package storage

import (
	"context"
	"fmt"
	"time"
)

// Queries over install state (migrations/0006) and the logs it is derived
// from. Install filters work differently from the rollup-backed queries:
// installs carry no command or CI dimension, so those filters don't apply,
// and cli_version means "first seen on this version" for cohort queries
// (like Retention) and "moved to this version" for version changes. os and
// arch are the install's most recent values.

// bucketOf is rebucket's counterpart for a timestamp column of a plain
// table. Like rebucket, the expression is built only from validated
// literals, never from request input.
func bucketOf(b Bucket, col string) (string, error) {
	if _, ok := bucketRank[b]; !ok {
		return "", fmt.Errorf("storage: unsupported bucket %q", b)
	}
	return fmt.Sprintf("date_trunc('%s', %s, 'UTC')", b, col), nil
}

// installCohortPredicates selects the installs first seen in f.Time.
func installCohortPredicates(scope TenantScope, f Filters) *predicateBuilder {
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("first_seen >= $%d", f.Time.From)
	pb.add("first_seen < $%d", f.Time.To)
	pb.addIfAnyStr("first_version", f.CLIVersions)
	pb.addIfAnyStr("last_os", f.OSes)
	pb.addIfAnyStr("last_arch", f.Arches)
	return pb
}

// --- funnel -------------------------------------------------------------

// FunnelPoint is one cohort bucket of the install funnel: of the installs
// first seen in the bucket, how many have since run a command, and how many
// have had one succeed. Later stages are counted whenever they happened, even
// after the query window, so the most recent cohorts are still maturing.
type FunnelPoint struct {
	Bucket     time.Time `json:"bucket"`
	Installs   int64     `json:"installs"`
	RanCommand int64     `json:"ran_command"`
	Succeeded  int64     `json:"succeeded"`
}

// InstallFunnel groups installs by the bucket they were first seen in. Only
// installs that reported an install id (basic consent tier or above) are
// tracked at all, so these counts are a floor on real installs; see
// InstallCoverage.
func (q *QueryBuilder) InstallFunnel(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]FunnelPoint, error) {
	bucketExpr, err := bucketOf(bucket, "first_seen")
	if err != nil {
		return nil, err
	}
	pb := installCohortPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, count(*) AS installs,
       count(first_command_at) AS ran_command,
       count(first_success_at) AS succeeded
FROM installs
WHERE %s
GROUP BY 1
ORDER BY 1 ASC
LIMIT %s OFFSET %s`, bucketExpr, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: install funnel: %w", err)
	}
	defer rows.Close()

	var out []FunnelPoint
	for rows.Next() {
		var r FunnelPoint
		if err := rows.Scan(&r.Bucket, &r.Installs, &r.RanCommand, &r.Succeeded); err != nil {
			return nil, fmt.Errorf("storage: scan install funnel: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- activation time ----------------------------------------------------

// ActivationPoint describes time to first success for one cohort bucket. The
// percentiles are over installs that have activated only, so they say how
// fast the successful ones were, not how long the rest will take; read them
// with ActivatedCount against CohortSize. They are nil when nobody in the
// bucket has activated.
type ActivationPoint struct {
	Bucket         time.Time `json:"bucket"`
	CohortSize     int64     `json:"cohort_size"`
	ActivatedCount int64     `json:"activated_count"`
	P50Seconds     *float64  `json:"p50_seconds"`
	P90Seconds     *float64  `json:"p90_seconds"`
}

// InstallActivation measures first_success_at - first_seen per cohort bucket.
func (q *QueryBuilder) InstallActivation(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]ActivationPoint, error) {
	bucketExpr, err := bucketOf(bucket, "first_seen")
	if err != nil {
		return nil, err
	}
	pb := installCohortPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, count(*) AS cohort_size, count(first_success_at) AS activated_count,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM first_success_at - first_seen)),
       percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM first_success_at - first_seen))
FROM installs
WHERE %s
GROUP BY 1
ORDER BY 1 ASC
LIMIT %s OFFSET %s`, bucketExpr, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: install activation: %w", err)
	}
	defer rows.Close()

	var out []ActivationPoint
	for rows.Next() {
		var r ActivationPoint
		if err := rows.Scan(&r.Bucket, &r.CohortSize, &r.ActivatedCount, &r.P50Seconds, &r.P90Seconds); err != nil {
			return nil, fmt.Errorf("storage: scan install activation: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- version changes ----------------------------------------------------

// VersionChangePoint counts installs moving between two versions in a bucket.
type VersionChangePoint struct {
	Bucket      time.Time `json:"bucket"`
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	Direction   string    `json:"direction"` // upgrade | downgrade
	Changes     int64     `json:"changes"`
	Installs    int64     `json:"installs"`
}

// VersionChanges reads install_version_changes. cli_version filters on the
// version moved to; os and arch on the install's most recent values.
func (q *QueryBuilder) VersionChanges(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]VersionChangePoint, error) {
	bucketExpr, err := bucketOf(bucket, "c.at")
	if err != nil {
		return nil, err
	}
	pb := newScopedPredicates(scope, "c.tenant_id")
	pb.add("c.at >= $%d", f.Time.From)
	pb.add("c.at < $%d", f.Time.To)
	pb.addIfAnyStr("c.to_version", f.CLIVersions)
	pb.addIfAnyStr("i.last_os", f.OSes)
	pb.addIfAnyStr("i.last_arch", f.Arches)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	// A change whose install has since been swept still counts: the LEFT JOIN
	// only matters for the os/arch filters.
	sql := fmt.Sprintf(`
SELECT %s AS bucket, c.from_version, c.to_version, c.direction,
       count(*) AS changes, count(DISTINCT c.install_id) AS installs
FROM install_version_changes c
LEFT JOIN installs i ON i.tenant_id = c.tenant_id AND i.install_id = c.install_id
WHERE %s
GROUP BY 1, 2, 3, 4
ORDER BY 1 ASC, 2 ASC, 3 ASC
LIMIT %s OFFSET %s`, bucketExpr, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: version changes: %w", err)
	}
	defer rows.Close()

	var out []VersionChangePoint
	for rows.Next() {
		var r VersionChangePoint
		if err := rows.Scan(&r.Bucket, &r.FromVersion, &r.ToVersion, &r.Direction, &r.Changes, &r.Installs); err != nil {
			return nil, fmt.Errorf("storage: scan version changes: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- coverage -----------------------------------------------------------

// CoveragePoint says how much of the session traffic the install-based
// metrics can see: sessions that carried an install id (basic consent tier or
// above) against all sessions.
type CoveragePoint struct {
	Bucket          time.Time `json:"bucket"`
	SessionCount    int64     `json:"session_count"`
	TrackedSessions int64     `json:"tracked_sessions"`
}

// InstallCoverage reads raw session-start logs, so it is bounded by log
// retention.
func (q *QueryBuilder) InstallCoverage(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]CoveragePoint, error) {
	bucketExpr, err := logBucketExpr(bucket)
	if err != nil {
		return nil, err
	}
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("event_name = $%d", eventSessionStarted)
	pb.add("time >= $%d", f.Time.From)
	pb.add("time < $%d", f.Time.To)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, count(*) AS session_count,
       count(*) FILTER (WHERE log_attributes ? 'cli.install_id') AS tracked_sessions
FROM logs
WHERE %s
GROUP BY 1
ORDER BY 1 ASC
LIMIT %s OFFSET %s`, bucketExpr, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: install coverage: %w", err)
	}
	defer rows.Close()

	var out []CoveragePoint
	for rows.Next() {
		var r CoveragePoint
		if err := rows.Scan(&r.Bucket, &r.SessionCount, &r.TrackedSessions); err != nil {
			return nil, fmt.Errorf("storage: scan install coverage: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- install method -----------------------------------------------------

// InstallMethodPoint counts the installs first seen in a bucket that report a
// given install method. Method is nil for installs that never reported one
// (the vendor didn't set the SDK's install-method option, or the install
// predates it).
type InstallMethodPoint struct {
	Bucket   time.Time `json:"bucket"`
	Method   *string   `json:"install_method"`
	Installs int64     `json:"installs"`
}

// InstallMethods breaks the install cohorts down by vendor-reported install
// method (brew, npm, curl, binary, ...), using the latest value each install
// reported. Same cohort semantics and filters as InstallFunnel.
func (q *QueryBuilder) InstallMethods(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]InstallMethodPoint, error) {
	bucketExpr, err := bucketOf(bucket, "first_seen")
	if err != nil {
		return nil, err
	}
	pb := installCohortPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT %s AS bucket, install_method, count(*) AS installs
FROM installs
WHERE %s
GROUP BY 1, 2
ORDER BY 1 ASC, 2 ASC NULLS LAST
LIMIT %s OFFSET %s`, bucketExpr, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: install methods: %w", err)
	}
	defer rows.Close()

	var out []InstallMethodPoint
	for rows.Next() {
		var r InstallMethodPoint
		if err := rows.Scan(&r.Bucket, &r.Method, &r.Installs); err != nil {
			return nil, fmt.Errorf("storage: scan install methods: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- new vs returning ---------------------------------------------------

// NewReturningPoint splits the installs active in a bucket into those first
// seen in that very bucket and those first seen earlier.
type NewReturningPoint struct {
	Bucket    time.Time `json:"bucket"`
	New       int64     `json:"new_installs"`
	Returning int64     `json:"returning_installs"`
}

// NewVsReturning counts distinct installs that started a session in each
// bucket, split by whether the install's first sighting falls in that bucket.
// Activity comes from raw session logs, so it is bounded by log retention, and
// an install with no row in the installs table (not yet backfilled, or swept)
// can't be classified and is left out. "New" therefore means "first seen
// within install retention", like every install-based metric.
func (q *QueryBuilder) NewVsReturning(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]NewReturningPoint, error) {
	activeBucket, err := bucketOf(bucket, "l.time")
	if err != nil {
		return nil, err
	}
	firstBucket, err := bucketOf(bucket, "i.first_seen")
	if err != nil {
		return nil, err
	}

	pb := newScopedPredicates(scope, "l.tenant_id")
	pb.add("l.event_name = $%d", eventSessionStarted)
	pb.add("l.time >= $%d", f.Time.From)
	pb.add("l.time < $%d", f.Time.To)
	pb.clauses = append(pb.clauses, "l.log_attributes ? 'cli.install_id'")
	pb.addIfAnyStr("l.cli_version", f.CLIVersions)
	pb.addIfAnyStr("l.os", f.OSes)
	pb.addIfAnyStr("l.arch", f.Arches)
	pb.addIfNotNilBool("l.is_ci", f.IsCI)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
WITH active AS (
	SELECT DISTINCT %[1]s AS bucket, l.tenant_id, l.log_attributes ->> 'cli.install_id' AS install_id
	FROM logs l
	WHERE %[3]s
)
SELECT a.bucket,
       count(*) FILTER (WHERE %[2]s = a.bucket) AS new_installs,
       count(*) FILTER (WHERE %[2]s < a.bucket) AS returning_installs
FROM active a
JOIN installs i ON i.tenant_id = a.tenant_id AND i.install_id = a.install_id
GROUP BY a.bucket
ORDER BY a.bucket ASC
LIMIT %[4]s OFFSET %[5]s`, activeBucket, firstBucket, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: new vs returning: %w", err)
	}
	defer rows.Close()

	var out []NewReturningPoint
	for rows.Next() {
		var r NewReturningPoint
		if err := rows.Scan(&r.Bucket, &r.New, &r.Returning); err != nil {
			return nil, fmt.Errorf("storage: scan new vs returning: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
