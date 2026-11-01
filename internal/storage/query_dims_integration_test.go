package storage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAdmitCounterDims_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	w := NewWriter(pool)
	tenant := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	key := func(name, k, v string) DimValueKey { return DimValueKey{name, k, v} }

	// Limit 2 per (name, key): the two most frequent new checks are kept, the
	// third folds; severity has its own room.
	adm, err := w.AdmitCounterDims(ctx, tenant, map[DimValueKey]int64{
		key("vulns", "check", "bola"):    5,
		key("vulns", "check", "jwt"):     3,
		key("vulns", "check", "cors"):    1,
		key("vulns", "severity", "high"): 9,
		key("vulns", "check", "other"):   4,
	}, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []DimValueKey{key("vulns", "check", "bola"), key("vulns", "check", "jwt"), key("vulns", "severity", "high"), key("vulns", "check", "other")} {
		if !adm.Values[k] {
			t.Errorf("%v should be admitted", k)
		}
	}
	if adm.Values[key("vulns", "check", "cors")] || adm.Folded != 1 {
		t.Errorf("cors should fold: %+v", adm)
	}

	// Known values are admitted without a slot; the cap holds across batches
	// and is per name.
	adm, err = w.AdmitCounterDims(ctx, tenant, map[DimValueKey]int64{
		key("vulns", "check", "jwt"):   1,
		key("vulns", "check", "xss"):   1,
		key("other.n", "check", "xss"): 1,
	}, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if !adm.Values[key("vulns", "check", "jwt")] || adm.Values[key("vulns", "check", "xss")] || !adm.Values[key("other.n", "check", "xss")] || adm.Folded != 1 {
		t.Errorf("second batch: %+v", adm)
	}

	if err := w.RecordFoldedCounterDims(ctx, tenant, 3, now); err != nil {
		t.Fatal(err)
	}
	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	list, err := qb.Counters(ctx, scope, Filters{Time: TimeRange{From: now.Add(-time.Hour), To: now}})
	if err != nil || list.Overflow.FoldedDimValues != 3 || list.Overflow.LastFoldedAt == nil {
		t.Errorf("overflow = %+v err=%v, want 3 folded dimension values", list.Overflow, err)
	}
}

// TestDimQueries_Live covers group_by and where over stored tuples, plain
// labels and folded labels, for counters and measures.
func TestDimQueries_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant := uuid.New()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	h10, h11 := day.Add(10*time.Hour), day.Add(11*time.Hour)

	var rows []MetricRow
	counter := func(at time.Time, label string, v float64) {
		attrs := Attrs{"cli.counter.name": "vulns"}
		if label != "" {
			attrs["cli.counter.label"] = label
		}
		rows = append(rows, MetricRow{
			Time: at, TenantID: tenant, MetricName: "cli.counter", MetricType: "sum", Unit: strp("1"), Value: f64p(v),
			ConsentTier: "anonymous", DatapointAttributes: attrs,
		})
	}
	measure := func(label string, bucket int, n int64) {
		counts := make([]int64, MeasureBucketCount)
		counts[bucket] = n
		rows = append(rows, MetricRow{
			Time: h10, TenantID: tenant, MetricName: "cli.measure", MetricType: "histogram", Unit: strp("ms"),
			HistogramCount: &n, HistogramSum: f64p(float64(n)), HistogramMin: f64p(1), HistogramMax: f64p(1000),
			HistogramBucketBounds: MeasureBounds, HistogramBucketCounts: counts,
			ConsentTier: "anonymous", DatapointAttributes: Attrs{"cli.measure.name": "scan.run", "cli.measure.label": label},
		})
	}
	counter(h10, "check=bola;severity=high", 5)
	counter(h11, "check=bola;severity=high", 1)
	counter(h10, "check=jwt;severity=high", 2)
	counter(h10, "check=jwt;severity=low", 3)
	counter(h10, "check=other;severity=low", 4)
	counter(h10, "other", 7)  // whole tuple folded by the label cap
	counter(h10, "legacy", 8) // a plain label from an older SDK
	counter(h10, "", 10)      // no label at all
	measure("target=openapi", 3, 10)
	measure("target=graphql", 5, 2)
	measure("target=openapi", 5, 10)

	if _, err := NewWriter(pool).InsertMetrics(ctx, rows); err != nil {
		t.Fatal(err)
	}
	for _, cagg := range []string{"cagg_counters_hourly", "cagg_counters_daily", "cagg_measures_hourly", "cagg_measures_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}
	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	f := Filters{Time: TimeRange{From: day, To: day.AddDate(0, 0, 1)}}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	t.Run("group by one dimension", func(t *testing.T) {
		d, err := qb.CounterSeries(ctx, scope, f, "vulns", nil, DimQuery{GroupBy: []string{"severity"}}, BucketDay)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]float64{}
		for _, g := range d.Groups {
			got[str(g.Dims["severity"])] = g.Total
		}
		// low 7, high 8, other 7 (the folded tuple), <nil> 18 (legacy + none).
		want := map[string]float64{"high": 8, "low": 7, "other": 7, "<nil>": 18}
		if len(got) != len(want) {
			t.Fatalf("groups = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("severity %s = %v, want %v", k, got[k], v)
			}
		}
		if str(d.Groups[0].Dims["severity"]) != "<nil>" || d.Groups[1].Total != 8 {
			t.Errorf("groups should be largest first: %+v", d.Groups)
		}
	})

	t.Run("group by two dimensions, with a series per group", func(t *testing.T) {
		d, err := qb.CounterSeries(ctx, scope, f, "vulns", nil, DimQuery{GroupBy: []string{"severity", "check"}}, BucketHour)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range d.Groups {
			if str(g.Dims["severity"]) == "high" && str(g.Dims["check"]) == "bola" {
				if g.Total != 6 || len(g.Series) != 2 || g.Series[0].Total != 5 || g.Series[1].Total != 1 {
					t.Errorf("high/bola = %+v, want 5 then 1", g)
				}
				return
			}
		}
		t.Errorf("no high/bola group in %+v", d.Groups)
	})

	t.Run("where narrows series, labels and groups", func(t *testing.T) {
		d, err := qb.CounterSeries(ctx, scope, f, "vulns", nil,
			DimQuery{GroupBy: []string{"check"}, Where: map[string][]string{"severity": {"high"}}}, BucketDay)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Series) != 1 || d.Series[0].Total != 8 {
			t.Errorf("series = %+v, want 8", d.Series)
		}
		if len(d.Labels) != 2 {
			t.Errorf("labels = %+v, want the two high tuples", d.Labels)
		}
		if len(d.Groups) != 2 || str(d.Groups[0].Dims["check"]) != "bola" || d.Groups[0].Total != 6 {
			t.Errorf("groups = %+v, want bola 6, jwt 2", d.Groups)
		}
	})

	t.Run("a plain label is the label dimension", func(t *testing.T) {
		d, err := qb.CounterSeries(ctx, scope, f, "vulns", nil, DimQuery{Where: map[string][]string{"label": {"legacy"}}}, BucketDay)
		if err != nil || len(d.Series) != 1 || d.Series[0].Total != 8 {
			t.Errorf("label=legacy: %+v err=%v, want 8", d.Series, err)
		}
	})

	t.Run("no group_by leaves groups out", func(t *testing.T) {
		d, err := qb.CounterSeries(ctx, scope, f, "vulns", nil, DimQuery{}, BucketDay)
		if err != nil || d.Groups != nil || d.Series[0].Total != 40 {
			t.Errorf("plain detail: %+v err=%v", d, err)
		}
	})

	t.Run("measure groups merge their histograms", func(t *testing.T) {
		d, err := qb.MeasureSeries(ctx, scope, f, "scan.run", nil, DimQuery{GroupBy: []string{"target"}}, BucketDay)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Groups) != 2 {
			t.Fatalf("groups = %+v, want openapi and graphql", d.Groups)
		}
		g := d.Groups[0]
		if str(g.Dims["target"]) != "openapi" || g.Count != 20 || g.DatapointCount != 2 || g.P50 == nil || g.P95 == nil || *g.P95 <= *g.P50 {
			t.Errorf("openapi = %+v, want 20 durations over two buckets with p95 above p50", g)
		}
		if d.Groups[1].Count != 2 || len(d.Groups[1].Series) != 1 {
			t.Errorf("graphql = %+v", d.Groups[1])
		}
	})
}
