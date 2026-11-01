package storage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValues_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant := uuid.New()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	h10, h11 := day.Add(10*time.Hour), day.Add(11*time.Hour)

	var rows []MetricRow
	point := func(tenantID uuid.UUID, at time.Time, label string, buckets map[int]int64, sum, max float64) {
		counts := make([]int64, ValueBucketCount)
		var n int64
		for i, c := range buckets {
			counts[i] = c
			n += c
		}
		attrs := Attrs{"cli.value.name": "scan.findings", "cli.value.unit": "1"}
		if label != "" {
			attrs["cli.value.label"] = label
		}
		rows = append(rows, MetricRow{
			Time: at, TenantID: tenantID, MetricName: "cli.value", MetricType: "histogram", Unit: strp("1"),
			HistogramCount: &n, HistogramSum: f64p(sum), HistogramMin: f64p(0), HistogramMax: f64p(max),
			HistogramBucketBounds: ValueBounds, HistogramBucketCounts: counts,
			ConsentTier: "anonymous", ResourceAttributes: Attrs{"service.name": "vulnapi"}, DatapointAttributes: attrs,
		})
	}
	point(tenant, h10, "severity=high", map[int]int64{0: 6, 2: 2, 4: 2}, 14, 5)
	point(tenant, h11, "severity=low", map[int]int64{0: 2, 1: 8}, 8, 1)
	point(uuid.New(), h10, "", map[int]int64{1: 50}, 50, 1)
	if _, err := NewWriter(pool).InsertMetrics(ctx, rows); err != nil {
		t.Fatal(err)
	}
	for _, cagg := range []string{"cagg_values_hourly", "cagg_values_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}
	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	f := Filters{Time: TimeRange{From: day, To: day.AddDate(0, 0, 1)}, Services: []string{"vulnapi"}}

	list, err := qb.Values(ctx, scope, f)
	if err != nil || len(list.Values) != 1 {
		t.Fatalf("values = %+v err=%v", list, err)
	}
	v := list.Values[0]
	if v.Name != "scan.findings" || v.Unit != "1" || v.Count != 20 || v.LabelCount != 2 || *v.HitRate != 0.6 {
		t.Errorf("summary = %+v, want 20 runs, 2 labels, hit rate 0.6", v)
	}

	d, err := qb.ValueSeries(ctx, scope, f, "scan.findings", nil, DimQuery{GroupBy: []string{"severity"}}, BucketHour)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Series) != 2 || d.Series[0].Count != 10 || *d.Series[0].HitRate != 0.4 || d.Stats.Count != 20 || *d.Stats.HitRate != 0.6 {
		t.Errorf("series = %+v stats %+v", d.Series, d.Stats)
	}
	if len(d.Groups) != 2 || d.Groups[0].Count != 10 {
		t.Fatalf("groups = %+v", d.Groups)
	}
	for _, g := range d.Groups {
		if *g.Dims["severity"] == "low" && *g.HitRate != 0.8 {
			t.Errorf("low = %+v, want hit rate 0.8", g)
		}
	}

	services, err := qb.VendorServices(ctx, scope, Filters{Time: f.Time})
	if err != nil || len(services) != 1 || services[0].Values != 1 {
		t.Errorf("services = %+v err=%v, want vulnapi with one value", services, err)
	}
}
