package storage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReach_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant := uuid.New()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	h10, h11 := day.Add(10*time.Hour), day.Add(11*time.Hour)

	var rows []MetricRow
	counter := func(at time.Time, label, install string) {
		attrs := Attrs{"cli.counter.name": "vulns.found", "cli.counter.label": label}
		tier := "anonymous"
		if install != "" {
			attrs["cli.install_id"], tier = install, "basic"
		}
		rows = append(rows, MetricRow{Time: at, TenantID: tenant, MetricName: "cli.counter", MetricType: "sum", Value: f64p(1),
			ConsentTier: tier, ResourceAttributes: Attrs{"service.name": "vulnapi"}, DatapointAttributes: attrs})
	}
	counter(h10, "severity=high", "a")
	counter(h10, "severity=high", "b")
	counter(h11, "severity=high", "a") // a again: one install over the window
	counter(h11, "severity=low", "c")
	counter(h11, "severity=low", "") // anonymous: counted, but no install
	n := int64(1)
	counts := make([]int64, ValueBucketCount)
	counts[1] = 1
	rows = append(rows, MetricRow{Time: h10, TenantID: tenant, MetricName: "cli.value", MetricType: "histogram",
		HistogramCount: &n, HistogramSum: f64p(1), HistogramBucketBounds: ValueBounds, HistogramBucketCounts: counts,
		ConsentTier: "basic", ResourceAttributes: Attrs{"service.name": "vulnapi"}, DatapointAttributes: Attrs{"cli.value.name": "scan.findings", "cli.install_id": "a"}})
	if _, err := NewWriter(pool).InsertMetrics(ctx, rows); err != nil {
		t.Fatal(err)
	}
	var logs []LogRow
	for _, install := range []string{"a", "b", "c", "d", "e"} {
		logs = append(logs, LogRow{Time: h10, TenantID: tenant, EventName: "cli.session.started", ConsentTier: "basic", LogAttributes: Attrs{"cli.install_id": install}})
	}
	if _, err := NewWriter(pool).InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	for _, cagg := range []string{"cagg_counters_hourly", "cagg_counters_daily", "cagg_values_hourly", "cagg_values_daily", "cagg_session_activity_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}
	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	f := Filters{Time: TimeRange{From: day, To: day.AddDate(0, 0, 1)}}

	for _, bucket := range []Bucket{BucketHour, BucketDay} {
		d, err := qb.CounterSeries(ctx, scope, f, "vulns.found", nil, DimQuery{GroupBy: []string{"severity"}, Reach: true}, bucket)
		if err != nil {
			t.Fatal(err)
		}
		r := d.Reach
		if r == nil || r.InstallsReached != 3 || r.ActiveInstalls != 5 || *r.ActiveShare != 0.6 || *r.ReportedShare != 0.8 {
			t.Errorf("%s reach = %+v, want 3 of 5 active installs, 4 of 5 points with an install", bucket, r)
		}
		for _, g := range d.Groups {
			want := map[string]int64{"high": 2, "low": 1}[*g.Dims["severity"]]
			if g.InstallsReached == nil || *g.InstallsReached != want {
				t.Errorf("%s group %s installs = %v, want %d", bucket, *g.Dims["severity"], g.InstallsReached, want)
			}
		}
		if bucket == BucketHour && (len(d.Series) != 2 || *d.Series[0].InstallsReached != 2 || *d.Series[1].InstallsReached != 2) {
			t.Errorf("hourly series = %+v, want 2 installs each hour", d.Series)
		}
	}

	d, err := qb.CounterSeries(ctx, scope, f, "vulns.found", nil, DimQuery{}, BucketDay)
	if err != nil || d.Reach != nil || d.Series[0].InstallsReached != nil {
		t.Errorf("without reach = %+v err=%v, want no reach", d, err)
	}
	v, err := qb.ValueSeries(ctx, scope, f, "scan.findings", nil, DimQuery{Reach: true}, BucketDay)
	if err != nil || v.Reach == nil || v.Reach.InstallsReached != 1 {
		t.Errorf("value reach = %+v err=%v, want 1", v.Reach, err)
	}
}
