package storage

import (
	"context"
	"fmt"
	"time"
)

// Reach is how many installs a vendor metric reached over a window. Only data
// points sent at basic tier or above carry an install id, so InstallsReached
// is a lower bound: ReportedShare is the share of data points that carried
// one. ActiveInstalls is the tenant's distinct installs that started a
// session in the window, and ActiveShare InstallsReached over it. Both counts
// are HyperLogLog estimates (about 1% standard error).
type Reach struct {
	InstallsReached int64    `json:"installs_reached"`
	ReportedShare   *float64 `json:"reported_share"`
	ActiveInstalls  int64    `json:"active_installs"`
	ActiveShare     *float64 `json:"active_share"`
}

// reachResult is reach over a window, per bucket and per dimension group
// (keyed by groupKey of the group's values).
type reachResult struct {
	window    Reach
	perBucket map[time.Time]int64
	perGroup  map[string]int64
}

// reach counts the installs behind one metric's rows of table (a vendor
// rollup with install_hll), with predicates building its WHERE clause.
func (q *QueryBuilder) reach(ctx context.Context, scope TenantScope, f Filters, table, bucketExpr, labelCol string, predicates func() *predicateBuilder, dq DimQuery) (reachResult, error) {
	res := reachResult{perBucket: map[time.Time]int64{}, perGroup: map[string]int64{}}

	pb := predicates()
	var points, withInstall *int64
	var reached *int64
	if err := q.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT distinct_count(rollup(install_hll))::bigint, sum(install_datapoint_count)::bigint, sum(datapoint_count)::bigint
FROM %s WHERE %s`, table, pb.where()), pb.args...).Scan(&reached, &withInstall, &points); err != nil {
		return res, fmt.Errorf("storage: reach: %w", err)
	}
	if reached != nil {
		res.window.InstallsReached = *reached
	}
	if points != nil && *points > 0 && withInstall != nil {
		share := float64(*withInstall) / float64(*points)
		res.window.ReportedShare = &share
	}

	ap := sessionActivityPredicates(scope, f)
	var active *int64
	if err := q.pool.QueryRow(ctx, fmt.Sprintf(`SELECT distinct_count(rollup(install_hll))::bigint FROM %s WHERE %s`,
		caggSessionActivityDaily, ap.where()), ap.args...).Scan(&active); err != nil {
		return res, fmt.Errorf("storage: reach active installs: %w", err)
	}
	if active != nil {
		res.window.ActiveInstalls = *active
		if *active > 0 {
			share := float64(res.window.InstallsReached) / float64(*active)
			res.window.ActiveShare = &share
		}
	}

	pb = predicates()
	rows, err := q.pool.Query(ctx, fmt.Sprintf(`
SELECT %s AS bucket, distinct_count(rollup(install_hll))::bigint FROM %s WHERE %s GROUP BY 1`, bucketExpr, table, pb.where()), pb.args...)
	if err != nil {
		return res, fmt.Errorf("storage: reach per bucket: %w", err)
	}
	for rows.Next() {
		var b time.Time
		var n int64
		if err := rows.Scan(&b, &n); err != nil {
			rows.Close()
			return res, fmt.Errorf("storage: scan reach: %w", err)
		}
		res.perBucket[b] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("storage: reach per bucket: %w", err)
	}

	if len(dq.GroupBy) == 0 {
		return res, nil
	}
	pb = predicates()
	exprs := dq.groupExprs(pb, labelCol)
	cols := ""
	for i, e := range exprs {
		if i > 0 {
			cols += ", "
		}
		cols += e
	}
	rows, err = q.pool.Query(ctx, fmt.Sprintf(`
SELECT %s, distinct_count(rollup(install_hll))::bigint FROM %s WHERE %s GROUP BY %s`, cols, table, pb.where(), groupByOrdinals(len(exprs))), pb.args...)
	if err != nil {
		return res, fmt.Errorf("storage: reach per group: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		values := make([]*string, len(exprs))
		var n int64
		dest := make([]any, 0, len(values)+1)
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(append(dest, &n)...); err != nil {
			return res, fmt.Errorf("storage: scan reach per group: %w", err)
		}
		res.perGroup[groupKey(values)] = n
	}
	return res, rows.Err()
}
