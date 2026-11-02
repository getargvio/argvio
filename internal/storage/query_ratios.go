package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// RatioOperand is one side of a ratio metric, resolved against the catalog:
// a vendor metric of the CLI (Kind counter, duration or value) narrowed by
// dimension values, or a built-in series.
type RatioOperand struct {
	// Kind is counter, duration or value for a metric, "" for a builtin.
	Kind   string
	Metric string
	// Builtin is command_invocations, sessions or active_installs.
	Builtin string
	// Where narrows a metric by dimension values, or command_invocations by
	// command name ("command").
	Where map[string]string
	// Agg is how a value metric is read: sum (default) or count.
	Agg string
}

// Metric-backed ratio operand kinds.
const (
	ratioKindCounter  = "counter"
	ratioKindDuration = "duration"
	ratioKindValue    = "value"
)

// ratioGrainDailyOnly marks operands whose rollup only exists at daily
// granularity, so the query can't use the hourly tables.
const ratioGrainDailyOnly = "daily-only"

// Built-in ratio operands.
const (
	RatioBuiltinInvocations    = "command_invocations"
	RatioBuiltinSessions       = "sessions"
	RatioBuiltinActiveInstalls = "active_installs"
)

// RatioNeedsDailyBucket reports whether o reads a daily-only rollup, so the
// ratio can't be bucketed by hour.
func (o RatioOperand) RatioNeedsDailyBucket() bool {
	return o.Builtin == RatioBuiltinSessions || o.Builtin == RatioBuiltinActiveInstalls
}

// RatioPoint is a ratio's two sides and value over one bucket or the whole
// window. Value is nil when the denominator is zero.
type RatioPoint struct {
	Bucket      *time.Time `json:"bucket,omitempty"`
	Numerator   float64    `json:"numerator"`
	Denominator float64    `json:"denominator"`
	Value       *float64   `json:"value"`
}

// RatioSeries is one ratio over a window: per bucket and in total.
type RatioSeries struct {
	Series []RatioPoint `json:"series"`
	Total  RatioPoint   `json:"total"`
}

// Ratio evaluates num/den × scale per bucket and over the whole window, with
// the same filters on both sides. Vendor-metric operands are scoped to
// service; the built-in series cover the whole tenant (narrow
// command_invocations with a command). Buckets where either side has data are
// listed, ascending.
func (q *QueryBuilder) Ratio(ctx context.Context, scope TenantScope, f Filters, service string, num, den RatioOperand, scale float64, bucket Bucket) (RatioSeries, error) {
	nb, nt, err := q.operandSeries(ctx, scope, f, service, num, bucket)
	if err != nil {
		return RatioSeries{}, err
	}
	db, dt, err := q.operandSeries(ctx, scope, f, service, den, bucket)
	if err != nil {
		return RatioSeries{}, err
	}
	keys := map[time.Time]bool{}
	for k := range nb {
		keys[k] = true
	}
	for k := range db {
		keys[k] = true
	}
	buckets := make([]time.Time, 0, len(keys))
	for k := range keys {
		buckets = append(buckets, k)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Before(buckets[j]) })
	out := RatioSeries{Series: make([]RatioPoint, 0, len(buckets)), Total: ratioPoint(nil, nt, dt, scale)}
	for _, b := range buckets {
		b := b
		out.Series = append(out.Series, ratioPoint(&b, nb[b], db[b], scale))
	}
	return out, nil
}

func ratioPoint(bucket *time.Time, n, d, scale float64) RatioPoint {
	p := RatioPoint{Bucket: bucket, Numerator: n, Denominator: d}
	if d != 0 {
		v := n / d * scale
		p.Value = &v
	}
	return p
}

// operandSeries returns an operand's value per bucket and over the window.
// For every additive operand the window total is the sum of the buckets;
// active installs are a distinct count, so their total is queried apart.
func (q *QueryBuilder) operandSeries(ctx context.Context, scope TenantScope, f Filters, service string, o RatioOperand, bucket Bucket) (map[time.Time]float64, float64, error) {
	var table, valueExpr, grain string
	var pb *predicateBuilder
	labelCol := ""
	switch {
	case o.Kind == ratioKindCounter:
		table, valueExpr, labelCol = caggCountersHourly, "sum(total)", "counter_label"
		pb = q.metricOperandPredicates(scope, f, service, "counter_name", o.Metric)
	case o.Kind == ratioKindDuration:
		table, valueExpr, labelCol = caggMeasuresHourly, "sum(measure_count)", "measure_label"
		pb = q.metricOperandPredicates(scope, f, service, "measure_name", o.Metric)
	case o.Kind == ratioKindValue:
		valueExpr = "sum(value_sum)"
		if o.Agg == "count" {
			valueExpr = "sum(value_count)"
		}
		table, labelCol = caggValuesHourly, "value_label"
		pb = q.metricOperandPredicates(scope, f, service, "value_name", o.Metric)
	case o.Builtin == RatioBuiltinInvocations:
		table, valueExpr = caggCommandStatsHourly, "sum(invocation_count)"
		if c, ok := o.Where["command"]; ok {
			f.CommandNames = []string{c}
		}
		pb = commandStatsPredicates(scope, f)
	case o.Builtin == RatioBuiltinSessions:
		table, valueExpr, grain = caggSessionActivityDaily, "sum(session_count)", ratioGrainDailyOnly
		pb = sessionActivityPredicates(scope, f)
	case o.Builtin == RatioBuiltinActiveInstalls:
		table, valueExpr, grain = caggSessionActivityDaily, "distinct_count(rollup(install_hll))", ratioGrainDailyOnly
		pb = sessionActivityPredicates(scope, f)
	default:
		return nil, 0, fmt.Errorf("storage: unknown ratio operand %+v", o)
	}
	if labelCol != "" {
		where := map[string][]string{}
		for k, v := range o.Where {
			where[k] = []string{v}
		}
		DimQuery{Where: where}.addWhere(pb, labelCol)
	}

	var bucketExpr string
	var err error
	switch {
	case grain == ratioGrainDailyOnly:
		bucketExpr, err = rebucket(bucket, BucketDay)
	case bucket == BucketHour:
		bucketExpr = bucketColumn
	default:
		bucketExpr, err = rebucket(bucket, BucketDay)
		table = dailyOf(table)
	}
	if err != nil {
		return nil, 0, err
	}

	rows, err := q.pool.Query(ctx, fmt.Sprintf(`
SELECT %s AS bucket, (%s)::double precision FROM %s WHERE %s GROUP BY 1`, bucketExpr, valueExpr, table, pb.where()), pb.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: ratio operand: %w", err)
	}
	out := map[time.Time]float64{}
	var total float64
	for rows.Next() {
		var b time.Time
		var v *float64
		if err := rows.Scan(&b, &v); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("storage: scan ratio operand: %w", err)
		}
		if v != nil {
			out[b] = *v
			total += *v
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("storage: ratio operand: %w", err)
	}
	if o.Builtin == RatioBuiltinActiveInstalls {
		var v *float64
		if err := q.pool.QueryRow(ctx, fmt.Sprintf(`SELECT (%s)::double precision FROM %s WHERE %s`, valueExpr, table, pb.where()), pb.args...).Scan(&v); err != nil {
			return nil, 0, fmt.Errorf("storage: ratio active installs: %w", err)
		}
		total = 0
		if v != nil {
			total = *v
		}
	}
	return out, total, nil
}

func (q *QueryBuilder) metricOperandPredicates(scope TenantScope, f Filters, service, nameCol, name string) *predicateBuilder {
	f.Services = []string{service}
	pb := vendorPredicates(scope, f)
	pb.add(nameCol+" = $%d", name)
	return pb
}

// dailyOf maps an hourly rollup to its daily one.
func dailyOf(hourly string) string {
	switch hourly {
	case caggCountersHourly:
		return caggCountersDaily
	case caggMeasuresHourly:
		return caggMeasuresDaily
	case caggValuesHourly:
		return caggValuesDaily
	case caggCommandStatsHourly:
		return caggCommandStatsDaily
	}
	return hourly
}

// ActiveMetricDefinitions returns the declared, non-deprecated definitions of
// one tenant's CLI by name, as their stored specs.
func (q *QueryBuilder) ActiveMetricDefinitions(ctx context.Context, scope TenantScope, service string) (map[string]json.RawMessage, error) {
	rows, err := q.pool.Query(ctx, `
SELECT name, spec FROM metric_definitions
WHERE tenant_id = $1 AND service_name = $2 AND deprecated_at IS NULL`, scope.tenantID, service)
	if err != nil {
		return nil, fmt.Errorf("storage: metric definitions: %w", err)
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var name string
		var spec []byte
		if err := rows.Scan(&name, &spec); err != nil {
			return nil, fmt.Errorf("storage: scan metric definitions: %w", err)
		}
		out[name] = spec
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
