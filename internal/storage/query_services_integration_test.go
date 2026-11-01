package storage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVendorMetricsByService_Live covers the service_name column of the
// vendor-metric rollups (migration 0015): two CLIs of one tenant sending the
// same counter and measure names are summed without a service filter and
// reported apart with one, and VendorServices lists them.
func TestVendorMetricsByService_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant, other := uuid.New(), uuid.New()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	h10 := day.Add(10 * time.Hour)

	var rows []MetricRow
	resource := func(service string) Attrs {
		if service == "" {
			return Attrs{}
		}
		return Attrs{"service.name": service}
	}
	counter := func(tenantID uuid.UUID, service, name string, v float64) {
		rows = append(rows, MetricRow{
			Time: h10, TenantID: tenantID, MetricName: "cli.counter", MetricType: "sum", Unit: strp("1"), Value: f64p(v),
			CommandName: strp("scan"), CLIVersion: strp("1.0.0"), OS: strp("linux"), Arch: strp("amd64"), IsCI: boolp(false),
			ConsentTier: "anonymous", ResourceAttributes: resource(service), DatapointAttributes: Attrs{"cli.counter.name": name},
		})
	}
	measure := func(tenantID uuid.UUID, service, name string, ms float64) {
		counts := make([]int64, MeasureBucketCount)
		counts[3] = 1 // (10, 50] ms
		n := int64(1)
		rows = append(rows, MetricRow{
			Time: h10, TenantID: tenantID, MetricName: "cli.measure", MetricType: "histogram", Unit: strp("ms"),
			HistogramCount: &n, HistogramSum: f64p(ms), HistogramMin: f64p(ms), HistogramMax: f64p(ms),
			HistogramBucketBounds: MeasureBounds, HistogramBucketCounts: counts,
			CommandName: strp("scan"), CLIVersion: strp("1.0.0"), OS: strp("linux"), Arch: strp("amd64"), IsCI: boolp(false),
			ConsentTier: "anonymous", ResourceAttributes: resource(service), DatapointAttributes: Attrs{"cli.measure.name": name},
		})
	}
	counter(tenant, "vulnapi", "findings", 5)
	counter(tenant, "vulnapi", "operations", 40)
	counter(tenant, "jwtop", "findings", 2)
	counter(tenant, "", "findings", 1)
	measure(tenant, "jwtop", "crack.run", 20)
	measure(tenant, "vulnapi", "crack.run", 30)
	counter(other, "vulnapi", "findings", 100)

	if _, err := NewWriter(pool).InsertMetrics(ctx, rows); err != nil {
		t.Fatalf("insert metrics: %v", err)
	}
	for _, cagg := range []string{"cagg_counters_hourly", "cagg_counters_daily", "cagg_measures_hourly", "cagg_measures_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	rng := TimeRange{From: day, To: day.AddDate(0, 0, 1)}
	totals := func(t *testing.T, f Filters) map[string]float64 {
		t.Helper()
		list, err := qb.Counters(ctx, scope, f)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]float64{}
		for _, c := range list.Counters {
			m[c.Name] = c.Total
		}
		return m
	}

	t.Run("without a service filter every CLI is summed", func(t *testing.T) {
		if got := totals(t, Filters{Time: rng}); got["findings"] != 8 || got["operations"] != 40 {
			t.Errorf("totals = %v, want findings 8, operations 40", got)
		}
	})

	t.Run("a service filter keeps that CLI only", func(t *testing.T) {
		if got := totals(t, Filters{Time: rng, Services: []string{"jwtop"}}); len(got) != 1 || got["findings"] != 2 {
			t.Errorf("jwtop totals = %v, want findings 2 only", got)
		}
		if got := totals(t, Filters{Time: rng, Services: []string{"jwtop", "vulnapi"}}); got["findings"] != 7 {
			t.Errorf("jwtop+vulnapi totals = %v, want findings 7", got)
		}
		d, err := qb.CounterSeries(ctx, scope, Filters{Time: rng, Services: []string{"vulnapi"}}, "findings", nil, DimQuery{}, BucketHour)
		if err != nil || len(d.Series) != 1 || d.Series[0].Total != 5 {
			t.Errorf("vulnapi series = %+v err=%v, want 5", d.Series, err)
		}
	})

	t.Run("measures are filtered the same way", func(t *testing.T) {
		d, err := qb.MeasureSeries(ctx, scope, Filters{Time: rng, Services: []string{"jwtop"}}, "crack.run", nil, DimQuery{}, BucketDay)
		if err != nil || len(d.Series) != 1 || d.Series[0].Count != 1 || d.Series[0].SumMS != 20 {
			t.Errorf("jwtop measure series = %+v err=%v, want one duration of 20ms", d.Series, err)
		}
		list, err := qb.Measures(ctx, scope, Filters{Time: rng})
		if err != nil || len(list.Measures) != 1 || list.Measures[0].Count != 2 {
			t.Errorf("all measures = %+v err=%v, want crack.run over 2 durations", list.Measures, err)
		}
	})

	t.Run("services lists each CLI with its name counts", func(t *testing.T) {
		got, err := qb.VendorServices(ctx, scope, Filters{Time: rng, Services: []string{"ignored"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("services = %+v, want vulnapi, jwtop and the unnamed one", got)
		}
		if got[0].Name == nil || *got[0].Name != "vulnapi" || got[0].Counters != 2 || got[0].Measures != 1 {
			t.Errorf("first = %+v, want vulnapi with 2 counters, 1 measure", got[0])
		}
		if got[1].Name == nil || *got[1].Name != "jwtop" || got[1].Counters != 1 || got[1].Measures != 1 {
			t.Errorf("second = %+v, want jwtop with 1 counter, 1 measure", got[1])
		}
		if got[2].Name != nil || got[2].Counters != 1 {
			t.Errorf("third = %+v, want the data points without service.name", got[2])
		}
	})

	t.Run("another tenant's services are never listed", func(t *testing.T) {
		empty, _ := NewTenantScope(uuid.New())
		got, err := qb.VendorServices(ctx, empty, Filters{Time: rng})
		if err != nil || len(got) != 0 {
			t.Errorf("empty tenant: %+v err=%v", got, err)
		}
	})
}
