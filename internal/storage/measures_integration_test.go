package storage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// AdmitMeasures shares its implementation with AdmitCounters
// (TestAdmitCounters_Live covers the caps in depth); this checks that it runs
// against its own tables.
func TestAdmitMeasures_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	w := NewWriter(pool)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := uuid.New()
	key := func(name, label string) MeasureKey { return MeasureKey{name, label} }

	adm, err := w.AdmitMeasures(ctx, tenant, map[MeasureKey]int64{
		key("john.run", "hmac"): 5, key("john.run", "md5"): 2, key("scan", ""): 1, key("probe", ""): 1,
	}, 2, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	// Two names fit: john.run, then probe, which ties with scan on frequency
	// and sorts first. One label per name.
	if !adm.Names["john.run"] || !adm.Names["probe"] || adm.Names["scan"] {
		t.Errorf("names = %v, want john.run and probe kept, scan folded", adm.Names)
	}
	if !adm.Labels[key("john.run", "hmac")] || adm.Labels[key("john.run", "md5")] {
		t.Errorf("labels = %v, want hmac kept and md5 folded", adm.Labels)
	}
	if adm.FoldedNames != 1 || adm.FoldedLabels != 2 {
		t.Errorf("folded names=%d labels=%d, want 1 and 2", adm.FoldedNames, adm.FoldedLabels)
	}

	var names, labels, foldedNames, foldedLabels int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM measure_names WHERE tenant_id = $1`, tenant).Scan(&names); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM measure_labels WHERE tenant_id = $1`, tenant).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT folded_names, folded_labels FROM measure_overflow WHERE tenant_id = $1`, tenant).Scan(&foldedNames, &foldedLabels); err != nil {
		t.Fatal(err)
	}
	if names != 2 || labels != 1 || foldedNames != 1 || foldedLabels != 2 {
		t.Errorf("stored names=%d labels=%d overflow=%d/%d, want 2, 1, 1/2", names, labels, foldedNames, foldedLabels)
	}

	// Values no longer sent are swept, a label with its name.
	sweepTenant := uuid.New()
	old := now.AddDate(0, 0, -100)
	if _, err := w.AdmitMeasures(ctx, sweepTenant, map[MeasureKey]int64{key("stale", "x"): 1, key("kept", "old"): 1, key("kept", "new"): 1}, 5, 5, old); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AdmitMeasures(ctx, sweepTenant, map[MeasureKey]int64{key("kept", "new"): 1}, 5, 5, now); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepTenantInstalls(ctx, pool, sweepTenant, 90); err != nil {
		t.Fatal(err)
	}
	var left []string
	rows, err := pool.Query(ctx, `SELECT measure_name || '/' || label FROM measure_labels WHERE tenant_id = $1
UNION ALL SELECT measure_name FROM measure_names WHERE tenant_id = $1 ORDER BY 1`, sweepTenant)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		left = append(left, v)
	}
	rows.Close()
	if len(left) != 2 || left[0] != "kept" || left[1] != "kept/new" {
		t.Errorf("after sweep: %v, want kept and kept/new", left)
	}

	// Counters keep a separate vocabulary.
	var counters int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM counter_names WHERE tenant_id = $1`, tenant).Scan(&counters); err != nil || counters != 0 {
		t.Errorf("counter_names = %d (err %v), want 0", counters, err)
	}
}

func TestMeasures_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant, other := uuid.New(), uuid.New()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)

	var rows []MetricRow
	// point records a histogram data point whose observations fall in the
	// given buckets of MeasureBounds (bucket index -> count).
	point := func(tenantID uuid.UUID, at time.Time, name, label, command string, ci bool, min, max, sum float64, buckets map[int]int64) {
		attrs := Attrs{"cli.measure.name": name}
		if label != "" {
			attrs["cli.measure.label"] = label
		}
		counts := make([]int64, MeasureBucketCount)
		var n int64
		for i, c := range buckets {
			counts[i] = c
			n += c
		}
		rows = append(rows, MetricRow{
			Time: at, TenantID: tenantID, MetricName: "cli.measure", MetricType: "histogram", Unit: strp("ms"),
			HistogramCount: &n, HistogramSum: f64p(sum), HistogramMin: f64p(min), HistogramMax: f64p(max),
			HistogramBucketBounds: MeasureBounds, HistogramBucketCounts: counts,
			CommandName: strp(command), CLIVersion: strp("1.0.0"), OS: strp("linux"), Arch: strp("amd64"), IsCI: boolp(ci),
			ConsentTier: "anonymous", DatapointAttributes: attrs,
		})
	}
	h10, h11 := day.Add(10*time.Hour), day.Add(11*time.Hour)
	// john.run/hmac: 100 runs in (100, 500] across two exports, one from CI.
	point(tenant, h10, "john.run", "hmac", "crack", false, 120, 480, 30000, map[int]int64{5: 60})
	point(tenant, h10, "john.run", "hmac", "crack", true, 110, 490, 20000, map[int]int64{5: 40})
	// john.run/md5: 10 slow runs in (5000, 10000].
	point(tenant, h11, "john.run", "md5", "crack", false, 6000, 9000, 75000, map[int]int64{8: 10})
	// john.run without a label: 1 run under a millisecond.
	point(tenant, h11, "john.run", "", "crack", false, 0.4, 0.4, 0.4, map[int]int64{0: 1})
	point(tenant, h10, "probe", "", "scan", false, 20, 40, 300, map[int]int64{3: 10})
	point(other, h10, "leaked", "x", "scan", false, 1, 1, 1, map[int]int64{0: 1})
	point(other, h10, "john.run", "hmac", "crack", false, 9000, 9000, 9000, map[int]int64{8: 1})
	// Not a measure: must not reach the rollup.
	n := int64(1)
	rows = append(rows, MetricRow{Time: h10, TenantID: tenant, MetricName: "cli.command.duration", MetricType: "histogram",
		HistogramCount: &n, HistogramBucketBounds: MeasureBounds, HistogramBucketCounts: make([]int64, MeasureBucketCount),
		ConsentTier: "basic", DatapointAttributes: Attrs{"cli.measure.name": "john.run"}})

	if _, err := NewWriter(pool).InsertMetrics(ctx, rows); err != nil {
		t.Fatalf("insert metrics: %v", err)
	}
	for _, cagg := range []string{"cagg_measures_hourly", "cagg_measures_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO measure_overflow (tenant_id, folded_names, folded_labels, last_folded_at) VALUES ($1, 7, 2, $2)`, tenant, h11); err != nil {
		t.Fatal(err)
	}

	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	rng := TimeRange{From: day, To: day.AddDate(0, 0, 1)}
	byLabel := func(d MeasureDetail) map[string]MeasureLabelStats {
		m := map[string]MeasureLabelStats{}
		for _, l := range d.Labels {
			k := "<none>"
			if l.Label != nil {
				k = *l.Label
			}
			m[k] = l
		}
		return m
	}
	within := func(got *float64, lo, hi float64) bool { return got != nil && *got >= lo && *got <= hi }

	t.Run("list per name, most recorded first, with percentiles and the overflow", func(t *testing.T) {
		list, err := qb.Measures(ctx, scope, Filters{Time: rng})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Measures) != 2 {
			t.Fatalf("got %+v, want john.run and probe only", list.Measures)
		}
		j, p := list.Measures[0], list.Measures[1]
		if j.Name != "john.run" || j.Count != 111 || j.LabelCount != 2 || j.SumMS != 125000.4 || j.DatapointCount != 4 {
			t.Errorf("first = %+v, want john.run: 111 runs, 2 labels, 125000.4ms over 4 points", j)
		}
		if j.MinMS == nil || *j.MinMS != 0.4 || j.MaxMS == nil || *j.MaxMS != 9000 {
			t.Errorf("john.run min/max = %v/%v, want 0.4/9000", j.MinMS, j.MaxMS)
		}
		// 100 of 111 runs are in (100, 500]; the 10 slow ones are above p95.
		if !within(j.P50, 100, 500) || !within(j.P95, 5000, 9000) || !within(j.P99, 5000, 9000) {
			t.Errorf("john.run p50/p95/p99 = %v/%v/%v, want p50 in (100,500] and p95, p99 in (5000,9000]", j.P50, j.P95, j.P99)
		}
		if p.Name != "probe" || p.Count != 10 || p.LabelCount != 0 || !within(p.P50, 20, 40) {
			t.Errorf("second = %+v, want probe: 10 runs, no labels, p50 in 20..40", p)
		}
		if list.Overflow.FoldedNames != 7 || list.Overflow.FoldedLabels != 2 || list.Overflow.LastFoldedAt == nil {
			t.Errorf("overflow = %+v, want 7 names and 2 labels folded", list.Overflow)
		}
	})

	t.Run("list filters, and an empty overflow for a tenant without folds", func(t *testing.T) {
		ci := true
		list, err := qb.Measures(ctx, scope, Filters{Time: rng, IsCI: &ci})
		if err != nil || len(list.Measures) != 1 || list.Measures[0].Count != 40 {
			t.Errorf("is_ci=true: %+v err=%v, want john.run with 40 runs", list.Measures, err)
		}
		list, err = qb.Measures(ctx, scope, Filters{Time: rng, CommandNames: []string{"scan"}})
		if err != nil || len(list.Measures) != 1 || list.Measures[0].Name != "probe" {
			t.Errorf("command=scan: %+v err=%v", list.Measures, err)
		}
		list, err = qb.Measures(ctx, scope, Filters{Time: rng, CLIVersions: []string{"2.0.0"}, OSes: []string{"linux"}})
		if err != nil || len(list.Measures) != 0 {
			t.Errorf("version 2.0.0: %+v err=%v, want none", list.Measures, err)
		}
		otherScope, _ := NewTenantScope(uuid.New())
		list, err = qb.Measures(ctx, otherScope, Filters{Time: rng})
		if err != nil || len(list.Measures) != 0 || list.Overflow.FoldedNames != 0 || list.Overflow.LastFoldedAt != nil {
			t.Errorf("empty tenant: %+v err=%v", list, err)
		}
	})

	t.Run("series per hour, labels over the window", func(t *testing.T) {
		d, err := qb.MeasureSeries(ctx, scope, Filters{Time: rng}, "john.run", nil, BucketHour)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Series) != 2 || !d.Series[0].Bucket.Equal(h10) || d.Series[0].Count != 100 || d.Series[1].Count != 11 {
			t.Errorf("series = %+v, want 100 at 10:00 and 11 at 11:00", d.Series)
		}
		if !within(d.Series[0].P50, 100, 500) || !within(d.Series[1].P50, 5000, 10000) {
			t.Errorf("series p50 = %v then %v, want a fast hour then a slow one", d.Series[0].P50, d.Series[1].P50)
		}
		got := byLabel(d)
		if len(got) != 3 || got["hmac"].Count != 100 || got["md5"].Count != 10 || got["<none>"].Count != 1 {
			t.Errorf("labels = %+v, want hmac 100, md5 10, none 1", got)
		}
		if !within(got["hmac"].P95, 100, 500) || !within(got["md5"].P50, 5000, 9000) {
			t.Errorf("label percentiles hmac p95 %v, md5 p50 %v", got["hmac"].P95, got["md5"].P50)
		}
		if d.Labels[0].Label == nil || *d.Labels[0].Label != "hmac" {
			t.Errorf("labels should be most recorded first: %+v", d.Labels)
		}
	})

	t.Run("series per day, narrowed to a label", func(t *testing.T) {
		label := "md5"
		d, err := qb.MeasureSeries(ctx, scope, Filters{Time: rng}, "john.run", &label, BucketDay)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Series) != 1 || !d.Series[0].Bucket.Equal(day) || d.Series[0].Count != 10 || d.Series[0].DatapointCount != 1 {
			t.Errorf("series = %+v, want one day of 10 runs over 1 point", d.Series)
		}
		// The label breakdown ignores the label filter.
		if got := byLabel(d); len(got) != 3 {
			t.Errorf("labels = %v, want all three", got)
		}
	})

	t.Run("another tenant's measures are never returned", func(t *testing.T) {
		d, err := qb.MeasureSeries(ctx, scope, Filters{Time: rng}, "leaked", nil, BucketDay)
		if err != nil || len(d.Series) != 0 || len(d.Labels) != 0 {
			t.Errorf("leaked: %+v err=%v", d, err)
		}
	})

	t.Run("unsupported bucket is rejected", func(t *testing.T) {
		if _, err := qb.MeasureSeries(ctx, scope, Filters{Time: rng}, "john.run", nil, Bucket("minute")); err == nil {
			t.Fatal("expected error for unsupported bucket")
		}
	})
}
