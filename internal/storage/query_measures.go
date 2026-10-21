package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Vendor measure rollups (migrations/0014), same hourly/daily layering as the
// counter ones.
const (
	caggMeasuresHourly = "cagg_measures_hourly"
	caggMeasuresDaily  = "cagg_measures_daily"
)

// measureAggregates is the select list that sums a rollup's histogram columns
// over whatever the query groups by; measureAgg is what it scans into.
const measureAggregates = `sum(measure_count)::bigint, sum(datapoint_count)::bigint, sum(sum_ms), min(min_ms), max(max_ms),
       sum(bkt_0)::bigint, sum(bkt_1)::bigint, sum(bkt_2)::bigint, sum(bkt_3)::bigint, sum(bkt_4)::bigint, sum(bkt_5)::bigint, sum(bkt_6)::bigint, sum(bkt_7)::bigint, sum(bkt_8)::bigint, sum(bkt_9)::bigint, sum(bkt_10)::bigint, sum(bkt_11)::bigint`

type measureAgg struct {
	count   int64
	points  int64
	sum     *float64
	min     *float64
	max     *float64
	buckets [MeasureBucketCount]int64
}

func (a *measureAgg) dest() []any {
	d := []any{&a.count, &a.points, &a.sum, &a.min, &a.max}
	for i := range a.buckets {
		d = append(d, &a.buckets[i])
	}
	return d
}

// MeasureStats summarizes the durations (ms) behind a group of cli.measure
// data points. The percentiles are estimates interpolated from the fixed
// histogram buckets (MeasureBounds), nil if nothing was recorded; SumMS is
// the sum of the values the SDKs reported a sum for. Count is the number of
// recorded durations, DatapointCount the number of data points (roughly,
// process exports) they came in.
type MeasureStats struct {
	Count          int64    `json:"count"`
	DatapointCount int64    `json:"datapoint_count"`
	SumMS          float64  `json:"sum_ms"`
	MinMS          *float64 `json:"min_ms"`
	MaxMS          *float64 `json:"max_ms"`
	P50            *float64 `json:"p50_ms"`
	P95            *float64 `json:"p95_ms"`
	P99            *float64 `json:"p99_ms"`
}

func (a *measureAgg) stats() MeasureStats {
	s := MeasureStats{Count: a.count, DatapointCount: a.points, MinMS: a.min, MaxMS: a.max}
	if a.sum != nil {
		s.SumMS = *a.sum
	}
	s.P50 = measureQuantile(a.buckets, a.min, a.max, 0.50)
	s.P95 = measureQuantile(a.buckets, a.min, a.max, 0.95)
	s.P99 = measureQuantile(a.buckets, a.min, a.max, 0.99)
	return s
}

// MeasureSummary is one measure name over the query window. LabelCount is how
// many distinct labels it was reported with (including "other", not counting
// data points without a label).
type MeasureSummary struct {
	Name       string `json:"name"`
	LabelCount int64  `json:"label_count"`
	MeasureStats
}

// MeasureOverflow is how many cli.measure data points a tenant has had folded
// into "other" since its measures were first recorded: FoldedNames past
// public.max_measure_names_per_tenant, FoldedLabels past
// public.max_measure_labels_per_name. LastFoldedAt is nil if none ever were.
type MeasureOverflow struct {
	FoldedNames  int64      `json:"folded_names"`
	FoldedLabels int64      `json:"folded_labels"`
	LastFoldedAt *time.Time `json:"last_folded_at"`
}

// MeasureList is the response of Measures.
type MeasureList struct {
	Measures []MeasureSummary `json:"measures"`
	Overflow MeasureOverflow  `json:"overflow"`
}

// Measures lists the tenant's measure names with their statistics over
// f.Time, most recorded first, off the daily rollup, with the tenant's fold
// counts.
func (q *QueryBuilder) Measures(ctx context.Context, scope TenantScope, f Filters) (MeasureList, error) {
	pb := commandStatsPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
SELECT measure_name, count(DISTINCT measure_label), %s
FROM %s
WHERE %s
GROUP BY measure_name
ORDER BY 3 DESC, measure_name ASC
LIMIT %s OFFSET %s`, measureAggregates, caggMeasuresDaily, pb.where(), limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return MeasureList{}, fmt.Errorf("storage: measures: %w", err)
	}
	defer rows.Close()

	out := MeasureList{Measures: []MeasureSummary{}}
	for rows.Next() {
		var m MeasureSummary
		var a measureAgg
		if err := rows.Scan(append([]any{&m.Name, &m.LabelCount}, a.dest()...)...); err != nil {
			return MeasureList{}, fmt.Errorf("storage: scan measures: %w", err)
		}
		m.MeasureStats = a.stats()
		out.Measures = append(out.Measures, m)
	}
	if err := rows.Err(); err != nil {
		return MeasureList{}, fmt.Errorf("storage: measures: %w", err)
	}

	err = q.pool.QueryRow(ctx, `SELECT folded_names, folded_labels, last_folded_at FROM measure_overflow WHERE tenant_id = $1`, scope.tenantID).
		Scan(&out.Overflow.FoldedNames, &out.Overflow.FoldedLabels, &out.Overflow.LastFoldedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return MeasureList{}, fmt.Errorf("storage: measure overflow: %w", err)
	}
	return out, nil
}

// MeasurePoint is one bucket of a measure's time series.
type MeasurePoint struct {
	Bucket time.Time `json:"bucket"`
	MeasureStats
}

// MeasureLabelStats is one label's statistics over the query window. Label is
// nil for data points sent without one, and "other" for labels past the cap.
type MeasureLabelStats struct {
	Label *string `json:"label"`
	MeasureStats
}

// MeasureDetail is the response of MeasureSeries.
type MeasureDetail struct {
	Name   string              `json:"name"`
	Series []MeasurePoint      `json:"series"`
	Labels []MeasureLabelStats `json:"labels"`
}

// MeasureSeries returns one measure's statistics per bucket, narrowed to one
// label if label is non-nil, and per label over the whole window, most
// recorded first. f.Limit and f.Offset page the series; f.Limit also caps the
// labels. Both read the same rollup, so the label counts add up to the series
// count (without a label filter). Percentiles are estimated from summed
// bucket counts, so they are those of the window or bucket, never an average
// of percentiles.
func (q *QueryBuilder) MeasureSeries(ctx context.Context, scope TenantScope, f Filters, name string, label *string, bucket Bucket) (MeasureDetail, error) {
	table, grain := caggMeasuresHourly, BucketHour
	if bucket != BucketHour {
		table, grain = caggMeasuresDaily, BucketDay
	}
	bucketExpr, err := rebucket(bucket, grain)
	if err != nil {
		return MeasureDetail{}, err
	}
	out := MeasureDetail{Name: name, Series: []MeasurePoint{}, Labels: []MeasureLabelStats{}}

	pb := commandStatsPredicates(scope, f)
	pb.add("measure_name = $%d", name)
	if label != nil {
		pb.add("measure_label = $%d", *label)
	}
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)
	sql := fmt.Sprintf(`
SELECT %s AS bucket, %s
FROM %s
WHERE %s
GROUP BY 1
ORDER BY 1 ASC
LIMIT %s OFFSET %s`, bucketExpr, measureAggregates, table, pb.where(), limitPh, offsetPh)
	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return MeasureDetail{}, fmt.Errorf("storage: measure series: %w", err)
	}
	for rows.Next() {
		var p MeasurePoint
		var a measureAgg
		if err := rows.Scan(append([]any{&p.Bucket}, a.dest()...)...); err != nil {
			rows.Close()
			return MeasureDetail{}, fmt.Errorf("storage: scan measure series: %w", err)
		}
		p.MeasureStats = a.stats()
		out.Series = append(out.Series, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MeasureDetail{}, fmt.Errorf("storage: measure series: %w", err)
	}

	pb = commandStatsPredicates(scope, f)
	pb.add("measure_name = $%d", name)
	limitPh = pb.nextPlaceholder(clampLimit(f.Limit))
	sql = fmt.Sprintf(`
SELECT measure_label, %s
FROM %s
WHERE %s
GROUP BY 1
ORDER BY 2 DESC, 1 ASC NULLS LAST
LIMIT %s`, measureAggregates, table, pb.where(), limitPh)
	rows, err = q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return MeasureDetail{}, fmt.Errorf("storage: measure labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l MeasureLabelStats
		var a measureAgg
		if err := rows.Scan(append([]any{&l.Label}, a.dest()...)...); err != nil {
			return MeasureDetail{}, fmt.Errorf("storage: scan measure labels: %w", err)
		}
		l.MeasureStats = a.stats()
		out.Labels = append(out.Labels, l)
	}
	return out, rows.Err()
}
