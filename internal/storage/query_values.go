package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Vendor value rollups (migration 0021).
const (
	caggValuesHourly = "cagg_values_hourly"
	caggValuesDaily  = "cagg_values_daily"
)

// valueAggregates sums a values rollup's histogram columns over whatever the
// query groups by; valueAgg is what it scans into.
const valueAggregates = `sum(value_count)::bigint, sum(datapoint_count)::bigint, sum(value_sum), min(value_min), max(value_max),
       sum(bkt_0)::bigint, sum(bkt_1)::bigint, sum(bkt_2)::bigint, sum(bkt_3)::bigint, sum(bkt_4)::bigint, sum(bkt_5)::bigint, sum(bkt_6)::bigint, sum(bkt_7)::bigint, sum(bkt_8)::bigint, sum(bkt_9)::bigint, sum(bkt_10)::bigint, sum(bkt_11)::bigint, sum(bkt_12)::bigint, sum(bkt_13)::bigint, sum(bkt_14)::bigint`

type valueAgg struct {
	count   int64
	points  int64
	sum     *float64
	min     *float64
	max     *float64
	buckets [ValueBucketCount]int64
}

func (a *valueAgg) dest() []any {
	d := []any{&a.count, &a.points, &a.sum, &a.min, &a.max}
	for i := range a.buckets {
		d = append(d, &a.buckets[i])
	}
	return d
}

func (a *valueAgg) merge(b valueAgg) {
	a.count += b.count
	a.points += b.points
	if b.sum != nil {
		s := *b.sum
		if a.sum != nil {
			s += *a.sum
		}
		a.sum = &s
	}
	if b.min != nil && (a.min == nil || *b.min < *a.min) {
		m := *b.min
		a.min = &m
	}
	if b.max != nil && (a.max == nil || *b.max > *a.max) {
		m := *b.max
		a.max = &m
	}
	for i := range a.buckets {
		a.buckets[i] += b.buckets[i]
	}
}

// ValueStats summarizes the values behind a group of cli.value data points.
// Count is the number of recorded values (runs), DatapointCount the data
// points they came in. HitRate is the share of values above zero, exact from
// the (-inf, 0] bucket; percentiles are interpolated from the fixed buckets
// (ValueBounds). Mean, percentiles and HitRate are nil without values.
type ValueStats struct {
	Count          int64    `json:"count"`
	DatapointCount int64    `json:"datapoint_count"`
	Sum            float64  `json:"sum"`
	Mean           *float64 `json:"mean"`
	Min            *float64 `json:"min"`
	Max            *float64 `json:"max"`
	P50            *float64 `json:"p50"`
	P90            *float64 `json:"p90"`
	P99            *float64 `json:"p99"`
	HitRate        *float64 `json:"hit_rate"`
}

func (a *valueAgg) stats() ValueStats {
	s := ValueStats{Count: a.count, DatapointCount: a.points, Min: a.min, Max: a.max}
	if a.sum != nil {
		s.Sum = *a.sum
	}
	if a.count > 0 {
		mean := s.Sum / float64(a.count)
		hit := 1 - float64(a.buckets[0])/float64(a.count)
		s.Mean, s.HitRate = &mean, &hit
	}
	s.P50 = histQuantile(ValueBounds, a.buckets[:], a.min, a.max, 0.50)
	s.P90 = histQuantile(ValueBounds, a.buckets[:], a.min, a.max, 0.90)
	s.P99 = histQuantile(ValueBounds, a.buckets[:], a.min, a.max, 0.99)
	return s
}

// ValueSummary is one value name over the query window.
type ValueSummary struct {
	Name       string `json:"name"`
	Unit       string `json:"unit"`
	LabelCount int64  `json:"label_count"`
	ValueStats
}

// ValueOverflow is CounterOverflow for cli.value: data points folded into
// "other" since the tenant's values were first recorded.
type ValueOverflow struct {
	FoldedNames     int64      `json:"folded_names"`
	FoldedLabels    int64      `json:"folded_labels"`
	FoldedDimValues int64      `json:"folded_dim_values"`
	LastFoldedAt    *time.Time `json:"last_folded_at"`
}

// ValueList is the response of Values.
type ValueList struct {
	Values   []ValueSummary `json:"values"`
	Overflow ValueOverflow  `json:"overflow"`
}

// Values lists the tenant's value names with their statistics over f.Time,
// most recorded first, off the daily rollup, with the fold counts.
func (q *QueryBuilder) Values(ctx context.Context, scope TenantScope, f Filters) (ValueList, error) {
	pb := vendorPredicates(scope, f)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)
	sql := fmt.Sprintf(`
SELECT value_name, max(value_unit), count(DISTINCT value_label), %s
FROM %s
WHERE %s
GROUP BY value_name
ORDER BY 4 DESC, value_name ASC
LIMIT %s OFFSET %s`, valueAggregates, caggValuesDaily, pb.where(), limitPh, offsetPh)
	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return ValueList{}, fmt.Errorf("storage: values: %w", err)
	}
	defer rows.Close()
	out := ValueList{Values: []ValueSummary{}}
	for rows.Next() {
		var v ValueSummary
		var a valueAgg
		if err := rows.Scan(append([]any{&v.Name, &v.Unit, &v.LabelCount}, a.dest()...)...); err != nil {
			return ValueList{}, fmt.Errorf("storage: scan values: %w", err)
		}
		v.ValueStats = a.stats()
		out.Values = append(out.Values, v)
	}
	if err := rows.Err(); err != nil {
		return ValueList{}, fmt.Errorf("storage: values: %w", err)
	}
	err = q.pool.QueryRow(ctx, `SELECT folded_names, folded_labels, folded_dim_values, last_folded_at FROM value_overflow WHERE tenant_id = $1`, scope.tenantID).
		Scan(&out.Overflow.FoldedNames, &out.Overflow.FoldedLabels, &out.Overflow.FoldedDimValues, &out.Overflow.LastFoldedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ValueList{}, fmt.Errorf("storage: value overflow: %w", err)
	}
	return out, nil
}

// ValuePoint is one bucket of a value's time series.
type ValuePoint struct {
	Bucket time.Time `json:"bucket"`
	ValueStats
}

// ValueLabelStats is one label's (or dimension tuple's) statistics over the
// window; nil for data points without one.
type ValueLabelStats struct {
	Label *string `json:"label"`
	ValueStats
}

// ValueGroup is one combination of grouped-by dimension values.
type ValueGroup struct {
	Dims map[string]*string `json:"dims"`
	ValueStats
	Series []ValuePoint `json:"series"`
}

// ValueDetail is the response of ValueSeries. Stats covers the whole window
// (a merge of every bucket's histogram); Groups is only set with group_by.
type ValueDetail struct {
	Name   string            `json:"name"`
	Unit   string            `json:"unit"`
	Stats  ValueStats        `json:"stats"`
	Series []ValuePoint      `json:"series"`
	Labels []ValueLabelStats `json:"labels"`
	Groups []ValueGroup      `json:"groups,omitempty"`
}

// ValueSeries returns one value's statistics per bucket (and over the whole
// window), narrowed to one label if label is non-nil and by dq.Where, per
// label, and with dq.GroupBy per combination of dimension values, most
// recorded first.
func (q *QueryBuilder) ValueSeries(ctx context.Context, scope TenantScope, f Filters, name string, label *string, dq DimQuery, bucket Bucket) (ValueDetail, error) {
	table, grain := caggValuesHourly, BucketHour
	if bucket != BucketHour {
		table, grain = caggValuesDaily, BucketDay
	}
	bucketExpr, err := rebucket(bucket, grain)
	if err != nil {
		return ValueDetail{}, err
	}
	out := ValueDetail{Name: name, Unit: "1", Series: []ValuePoint{}, Labels: []ValueLabelStats{}}
	predicates := func(withLabel bool) *predicateBuilder {
		pb := vendorPredicates(scope, f)
		pb.add("value_name = $%d", name)
		if withLabel && label != nil {
			pb.add("value_label = $%d", *label)
		}
		dq.addWhere(pb, "value_label")
		return pb
	}

	pb := predicates(true)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)
	rows, err := q.pool.Query(ctx, fmt.Sprintf(`
SELECT %s AS bucket, max(value_unit), %s
FROM %s
WHERE %s
GROUP BY 1
ORDER BY 1 ASC
LIMIT %s OFFSET %s`, bucketExpr, valueAggregates, table, pb.where(), limitPh, offsetPh), pb.args...)
	if err != nil {
		return ValueDetail{}, fmt.Errorf("storage: value series: %w", err)
	}
	var window valueAgg
	for rows.Next() {
		var p ValuePoint
		var unit string
		var a valueAgg
		if err := rows.Scan(append([]any{&p.Bucket, &unit}, a.dest()...)...); err != nil {
			rows.Close()
			return ValueDetail{}, fmt.Errorf("storage: scan value series: %w", err)
		}
		out.Unit = unit
		p.ValueStats = a.stats()
		window.merge(a)
		out.Series = append(out.Series, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ValueDetail{}, fmt.Errorf("storage: value series: %w", err)
	}
	out.Stats = window.stats()

	pb = predicates(false)
	limitPh = pb.nextPlaceholder(clampLimit(f.Limit))
	rows, err = q.pool.Query(ctx, fmt.Sprintf(`
SELECT value_label, %s
FROM %s
WHERE %s
GROUP BY 1
ORDER BY 2 DESC, 1 ASC NULLS LAST
LIMIT %s`, valueAggregates, table, pb.where(), limitPh), pb.args...)
	if err != nil {
		return ValueDetail{}, fmt.Errorf("storage: value labels: %w", err)
	}
	for rows.Next() {
		var l ValueLabelStats
		var a valueAgg
		if err := rows.Scan(append([]any{&l.Label}, a.dest()...)...); err != nil {
			rows.Close()
			return ValueDetail{}, fmt.Errorf("storage: scan value labels: %w", err)
		}
		l.ValueStats = a.stats()
		out.Labels = append(out.Labels, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ValueDetail{}, fmt.Errorf("storage: value labels: %w", err)
	}

	if len(dq.GroupBy) == 0 {
		return out, nil
	}
	out.Groups, err = q.valueGroups(ctx, table, bucketExpr, predicates(true), dq, clampLimit(f.Limit))
	return out, err
}

func (q *QueryBuilder) valueGroups(ctx context.Context, table, bucketExpr string, pb *predicateBuilder, dq DimQuery, limit int) ([]ValueGroup, error) {
	exprs := dq.groupExprs(pb, "value_label")
	cols := ""
	for i, e := range exprs {
		cols += fmt.Sprintf(", %s AS g%d", e, i)
	}
	rows, err := q.pool.Query(ctx, fmt.Sprintf(`
SELECT %s AS bucket%s, %s
FROM %s
WHERE %s
GROUP BY %s
ORDER BY 1 ASC`, bucketExpr, cols, valueAggregates, table, pb.where(), groupByOrdinals(1+len(exprs))), pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: value groups: %w", err)
	}
	defer rows.Close()
	index := map[string]int{}
	var groups []ValueGroup
	var totals []valueAgg
	for rows.Next() {
		var p ValuePoint
		var a valueAgg
		values := make([]*string, len(exprs))
		dest := []any{&p.Bucket}
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(append(dest, a.dest()...)...); err != nil {
			return nil, fmt.Errorf("storage: scan value groups: %w", err)
		}
		p.ValueStats = a.stats()
		key := groupKey(values)
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, ValueGroup{Dims: dq.dimsOf(values), Series: []ValuePoint{}})
			totals = append(totals, valueAgg{})
		}
		totals[i].merge(a)
		groups[i].Series = append(groups[i].Series, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: value groups: %w", err)
	}
	for i := range groups {
		groups[i].ValueStats = totals[i].stats()
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Count > groups[j].Count })
	if len(groups) > limit {
		groups = groups[:limit]
	}
	if groups == nil {
		groups = []ValueGroup{}
	}
	return groups, nil
}
