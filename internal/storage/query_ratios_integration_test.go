package storage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRatio_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant := uuid.New()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	h10, h11, h12 := day.Add(10*time.Hour), day.Add(11*time.Hour), day.Add(12*time.Hour)
	res := Attrs{"service.name": "vulnapi"}

	var metrics []MetricRow
	counter := func(at time.Time, label string, v float64) {
		metrics = append(metrics, MetricRow{
			Time: at, TenantID: tenant, MetricName: "cli.counter", MetricType: "sum", Value: f64p(v), ConsentTier: "anonymous",
			ResourceAttributes: res, DatapointAttributes: Attrs{"cli.counter.name": "vulns.found", "cli.counter.label": label},
		})
	}
	value := func(at time.Time, runs int64, sum float64) {
		counts := make([]int64, ValueBucketCount)
		counts[7] = runs
		metrics = append(metrics, MetricRow{
			Time: at, TenantID: tenant, MetricName: "cli.value", MetricType: "histogram", Unit: strp("1"),
			HistogramCount: &runs, HistogramSum: f64p(sum), HistogramBucketBounds: ValueBounds, HistogramBucketCounts: counts,
			ConsentTier: "anonymous", ResourceAttributes: res, DatapointAttributes: Attrs{"cli.value.name": "scan.operations"},
		})
	}
	counter(h10, "severity=critical", 2)
	counter(h10, "severity=low", 8)
	counter(h11, "severity=critical", 3)
	counter(h12, "severity=critical", 1) // no operations at 12:00: denominator 0
	value(h10, 4, 200)
	value(h11, 2, 100)
	if _, err := NewWriter(pool).InsertMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	var traces []TraceRow
	for i, at := range []time.Time{h10, h10, h11, h11, h11} {
		traces = append(traces, TraceRow{
			Time: at.Add(time.Duration(i) * time.Minute), TenantID: tenant, TraceID: uuid.NewString(), SpanID: uuid.NewString(),
			SpanName: "cli.command.scan", StartTime: at, EndTime: at, StatusCode: 1, CommandName: strp("scan"), ConsentTier: "anonymous",
		})
	}
	traces = append(traces, TraceRow{Time: h10, TenantID: tenant, TraceID: uuid.NewString(), SpanID: uuid.NewString(),
		SpanName: "cli.command.init", StartTime: h10, EndTime: h10, StatusCode: 1, CommandName: strp("init"), ConsentTier: "anonymous"})
	if _, err := NewWriter(pool).InsertTraces(ctx, traces); err != nil {
		t.Fatal(err)
	}
	var logs []LogRow
	for i, install := range []string{"a", "b", "a"} {
		logs = append(logs, LogRow{Time: day.Add(time.Duration(i+1) * time.Hour), TenantID: tenant, EventName: "cli.session.started",
			ConsentTier: "basic", LogAttributes: Attrs{"cli.install_id": install}})
	}
	if _, err := NewWriter(pool).InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	for _, cagg := range []string{"cagg_counters_hourly", "cagg_counters_daily", "cagg_values_hourly", "cagg_values_daily",
		"cagg_command_stats_hourly", "cagg_command_stats_daily", "cagg_session_activity_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}
	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	f := Filters{Time: TimeRange{From: day, To: day.AddDate(0, 0, 1)}}
	vulns := RatioOperand{Kind: "counter", Metric: "vulns.found"}
	critical := RatioOperand{Kind: "counter", Metric: "vulns.found", Where: map[string]string{"severity": "critical"}}
	ops := RatioOperand{Kind: "value", Metric: "scan.operations", Agg: "sum"}

	t.Run("critical vulnerabilities per 100 operations, per hour", func(t *testing.T) {
		r, err := qb.Ratio(ctx, scope, f, "vulnapi", critical, ops, 100, BucketHour)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Series) != 3 {
			t.Fatalf("series = %+v, want 10:00, 11:00 and 12:00", r.Series)
		}
		if *r.Series[0].Value != 1 || *r.Series[1].Value != 3 || r.Series[2].Value != nil || r.Series[2].Numerator != 1 {
			t.Errorf("series = %+v, want 1, 3, then null over no operations", r.Series)
		}
		if r.Total.Numerator != 6 || r.Total.Denominator != 300 || *r.Total.Value != 2 {
			t.Errorf("total = %+v, want 6 / 300 × 100 = 2", r.Total)
		}
	})

	t.Run("per run, and per scan invocation", func(t *testing.T) {
		r, err := qb.Ratio(ctx, scope, f, "vulnapi", vulns, RatioOperand{Kind: "value", Metric: "scan.operations", Agg: "count"}, 1, BucketDay)
		if err != nil || r.Total.Denominator != 6 || *r.Total.Value != 14.0/6 {
			t.Errorf("per run = %+v err=%v, want 14 / 6 runs", r.Total, err)
		}
		r, err = qb.Ratio(ctx, scope, f, "vulnapi", vulns, RatioOperand{Builtin: RatioBuiltinInvocations, Where: map[string]string{"command": "scan"}}, 1, BucketDay)
		if err != nil || r.Total.Denominator != 5 {
			t.Errorf("per scan = %+v err=%v, want 5 scan invocations", r.Total, err)
		}
	})

	t.Run("sessions and active installs, daily", func(t *testing.T) {
		r, err := qb.Ratio(ctx, scope, f, "vulnapi", vulns, RatioOperand{Builtin: RatioBuiltinSessions}, 1, BucketDay)
		if err != nil || r.Total.Denominator != 3 {
			t.Errorf("per session = %+v err=%v, want 3 sessions", r.Total, err)
		}
		r, err = qb.Ratio(ctx, scope, f, "vulnapi", vulns, RatioOperand{Builtin: RatioBuiltinActiveInstalls}, 1, BucketDay)
		if err != nil || r.Total.Denominator != 2 {
			t.Errorf("per install = %+v err=%v, want 2 distinct installs", r.Total, err)
		}
		if _, err := qb.Ratio(ctx, scope, f, "vulnapi", vulns, RatioOperand{Builtin: RatioBuiltinSessions}, 1, BucketHour); err == nil {
			t.Error("sessions have no hour bucket")
		}
	})

	t.Run("another CLI's metrics are not counted", func(t *testing.T) {
		r, err := qb.Ratio(ctx, scope, f, "jwtop", vulns, ops, 1, BucketDay)
		if err != nil || r.Total.Numerator != 0 || r.Total.Value != nil {
			t.Errorf("jwtop = %+v err=%v, want nothing", r.Total, err)
		}
	})
}
