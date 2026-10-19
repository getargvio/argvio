package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Vendor counter rollups (migrations/0013), same hourly/daily layering as the
// command-stats ones.
const (
	caggCountersHourly = "cagg_counters_hourly"
	caggCountersDaily  = "cagg_counters_daily"
)

// CounterTotal is one counter name's total over the query window. LabelCount
// is how many distinct labels it was reported with (including "other", not
// counting data points without a label).
type CounterTotal struct {
	Name           string  `json:"name"`
	Total          float64 `json:"total"`
	DatapointCount int64   `json:"datapoint_count"`
	LabelCount     int64   `json:"label_count"`
}

// CounterOverflow is how many cli.counter data points a tenant has had folded
// into "other" since its counters were first recorded: FoldedNames past
// public.max_counter_names_per_tenant, FoldedLabels past
// public.max_counter_labels_per_name. LastFoldedAt is nil if none ever were.
type CounterOverflow struct {
	FoldedNames  int64      `json:"folded_names"`
	FoldedLabels int64      `json:"folded_labels"`
	LastFoldedAt *time.Time `json:"last_folded_at"`
}

// CounterList is the response of Counters.
type CounterList struct {
	Counters []CounterTotal  `json:"counters"`
	Overflow CounterOverflow `json:"overflow"`
}

// Counters lists the tenant's counter names with their totals over f.Time,
// largest first, off the daily rollup, with the tenant's fold counts.
func (q *QueryBuilder) Counters(ctx context.Context, scope TenantScope, f Filters) (CounterList, error) {
	pb := commandStatsPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT counter_name, sum(total) AS total, sum(datapoint_count) AS datapoint_count,
       count(DISTINCT counter_label) AS label_count
FROM %s
WHERE %s
GROUP BY counter_name
ORDER BY total DESC, counter_name ASC
LIMIT %s OFFSET %s`, caggCountersDaily, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return CounterList{}, fmt.Errorf("storage: counters: %w", err)
	}
	defer rows.Close()

	out := CounterList{Counters: []CounterTotal{}}
	for rows.Next() {
		var c CounterTotal
		if err := rows.Scan(&c.Name, &c.Total, &c.DatapointCount, &c.LabelCount); err != nil {
			return CounterList{}, fmt.Errorf("storage: scan counters: %w", err)
		}
		out.Counters = append(out.Counters, c)
	}
	if err := rows.Err(); err != nil {
		return CounterList{}, fmt.Errorf("storage: counters: %w", err)
	}

	err = q.pool.QueryRow(ctx, `SELECT folded_names, folded_labels, last_folded_at FROM counter_overflow WHERE tenant_id = $1`, scope.tenantID).
		Scan(&out.Overflow.FoldedNames, &out.Overflow.FoldedLabels, &out.Overflow.LastFoldedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CounterList{}, fmt.Errorf("storage: counter overflow: %w", err)
	}
	return out, nil
}

// CounterPoint is one bucket of a counter's time series.
type CounterPoint struct {
	Bucket         time.Time `json:"bucket"`
	Total          float64   `json:"total"`
	DatapointCount int64     `json:"datapoint_count"`
}

// CounterLabelTotal is one label's total over the query window. Label is nil
// for data points sent without one, and "other" for labels past the cap.
type CounterLabelTotal struct {
	Label *string `json:"label"`
	Total float64 `json:"total"`
}

// CounterDetail is the response of CounterSeries.
type CounterDetail struct {
	Name   string              `json:"name"`
	Series []CounterPoint      `json:"series"`
	Labels []CounterLabelTotal `json:"labels"`
}

// CounterSeries returns one counter's total per bucket, narrowed to one label
// if label is non-nil, and its labels over the whole window, largest first.
// f.Limit and f.Offset page the series; f.Limit also caps the labels. Both
// read the same rollup, so the label totals add up to the series total
// (without a label filter).
func (q *QueryBuilder) CounterSeries(ctx context.Context, scope TenantScope, f Filters, name string, label *string, bucket Bucket) (CounterDetail, error) {
	table, grain := caggCountersHourly, BucketHour
	if bucket != BucketHour {
		table, grain = caggCountersDaily, BucketDay
	}
	bucketExpr, err := rebucket(bucket, grain)
	if err != nil {
		return CounterDetail{}, err
	}
	out := CounterDetail{Name: name, Series: []CounterPoint{}, Labels: []CounterLabelTotal{}}

	pb := commandStatsPredicates(scope, f)
	pb.add("counter_name = $%d", name)
	if label != nil {
		pb.add("counter_label = $%d", *label)
	}
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)
	sql := fmt.Sprintf(`
SELECT %s AS bucket, sum(total) AS total, sum(datapoint_count) AS datapoint_count
FROM %s
WHERE %s
GROUP BY 1
ORDER BY 1 ASC
LIMIT %s OFFSET %s`, bucketExpr, table, pb.where(), limitPh, offsetPh)
	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return CounterDetail{}, fmt.Errorf("storage: counter series: %w", err)
	}
	for rows.Next() {
		var p CounterPoint
		if err := rows.Scan(&p.Bucket, &p.Total, &p.DatapointCount); err != nil {
			rows.Close()
			return CounterDetail{}, fmt.Errorf("storage: scan counter series: %w", err)
		}
		out.Series = append(out.Series, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return CounterDetail{}, fmt.Errorf("storage: counter series: %w", err)
	}

	pb = commandStatsPredicates(scope, f)
	pb.add("counter_name = $%d", name)
	limitPh = pb.nextPlaceholder(clampLimit(f.Limit))
	sql = fmt.Sprintf(`
SELECT counter_label, sum(total) AS total
FROM %s
WHERE %s
GROUP BY 1
ORDER BY 2 DESC, 1 ASC NULLS LAST
LIMIT %s`, table, pb.where(), limitPh)
	rows, err = q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return CounterDetail{}, fmt.Errorf("storage: counter labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l CounterLabelTotal
		if err := rows.Scan(&l.Label, &l.Total); err != nil {
			return CounterDetail{}, fmt.Errorf("storage: scan counter labels: %w", err)
		}
		out.Labels = append(out.Labels, l)
	}
	return out, rows.Err()
}
