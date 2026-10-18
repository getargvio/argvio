package storage

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestResourceUsage_Live covers the resource-usage rollups: percentiles per
// command and version, the "reported" sample size, runs that did not report,
// day re-bucketing, and tenant isolation.
func TestResourceUsage_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live resource usage test")
	}
	ctx := context.Background()
	if err := MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	tenant, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{tenant, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, id, id.String(), "t"); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	span := func(tenantID uuid.UUID, at time.Time, command, version string, rss, cpu *int64) TraceRow {
		return TraceRow{
			Time: at, TenantID: tenantID, TraceID: uuid.NewString(), SpanID: uuid.NewString(),
			SpanName: "cli.command." + command, StartTime: at, EndTime: at.Add(10 * time.Millisecond),
			DurationMs: 10, StatusCode: 1, CommandName: strp(command), ExitCode: i32p(0),
			CLIVersion: strp(version), OS: strp("linux"), Arch: strp("amd64"), IsCI: boolp(false),
			PeakRSSBytes: rss, CPUTimeMs: cpu, ConsentTier: "basic",
		}
	}

	var rows []TraceRow
	// deploy 1.1.0: 100 runs reporting both, spread over two hours of the
	// same day, rss 1..100 MB and cpu 1..100 ms, so every percentile is
	// known: p50 ~ 50, p95 ~ 95, p99 ~ 99.
	for i := 1; i <= 100; i++ {
		at := day.Add(10*time.Hour + time.Duration(i%2)*time.Hour)
		rows = append(rows, span(tenant, at, "deploy", "1.1.0", i64p(int64(i)*1_000_000), i64p(int64(i))))
	}
	// deploy 1.1.0: 20 more runs from a platform that cannot measure.
	for i := 0; i < 20; i++ {
		rows = append(rows, span(tenant, day.Add(10*time.Hour), "deploy", "1.1.0", nil, nil))
	}
	// deploy 1.0.0: nothing reported at all.
	for i := 0; i < 3; i++ {
		rows = append(rows, span(tenant, day.Add(10*time.Hour), "deploy", "1.0.0", nil, nil))
	}
	// status 1.1.0: memory reported, CPU not.
	for i := 1; i <= 5; i++ {
		rows = append(rows, span(tenant, day.Add(10*time.Hour), "status", "1.1.0", i64p(int64(i)*1_000_000), nil))
	}
	// Another tenant: must never show up.
	rows = append(rows, span(other, day.Add(10*time.Hour), "leak", "9.9.9", i64p(999), i64p(999)))

	if _, err := NewWriter(pool).InsertTraces(ctx, rows); err != nil {
		t.Fatalf("insert traces: %v", err)
	}
	for _, cagg := range []string{"cagg_command_resources_hourly", "cagg_command_resources_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	rng := TimeRange{From: day.Add(-time.Hour), To: day.AddDate(0, 0, 1)}
	byKey := func(points []ResourceUsagePoint) map[string]ResourceUsagePoint {
		m := map[string]ResourceUsagePoint{}
		for _, p := range points {
			v := "<nil>"
			if p.CLIVersion != nil {
				v = *p.CLIVersion
			}
			m[p.CommandName+"/"+v] = p
		}
		return m
	}
	near := func(t *testing.T, name string, got *float64, want float64) {
		t.Helper()
		if got == nil {
			t.Errorf("%s = nil, want ~%v", name, want)
			return
		}
		if math.Abs(*got-want) > want*0.03 {
			t.Errorf("%s = %v, want ~%v (3%%)", name, *got, want)
		}
	}

	t.Run("bucket=day merges hourly sketches into true percentiles", func(t *testing.T) {
		points, err := qb.ResourceUsage(ctx, scope, Filters{Time: rng}, BucketDay)
		if err != nil {
			t.Fatalf("ResourceUsage: %v", err)
		}
		if len(points) != 3 {
			t.Fatalf("got %d points, want 3 (deploy 1.1.0, deploy 1.0.0, status 1.1.0): %+v", len(points), points)
		}
		got := byKey(points)

		d := got["deploy/1.1.0"]
		if !d.Bucket.Equal(day) {
			t.Errorf("bucket = %v, want %v", d.Bucket, day)
		}
		if d.InvocationCount != 120 {
			t.Errorf("invocation_count = %d, want 120", d.InvocationCount)
		}
		if d.PeakRSSBytes.ReportedCount != 100 || d.CPUTimeMs.ReportedCount != 100 {
			t.Errorf("reported = rss %d cpu %d, want 100/100", d.PeakRSSBytes.ReportedCount, d.CPUTimeMs.ReportedCount)
		}
		near(t, "rss p50", d.PeakRSSBytes.P50, 50_000_000)
		near(t, "rss p95", d.PeakRSSBytes.P95, 95_000_000)
		near(t, "rss p99", d.PeakRSSBytes.P99, 99_000_000)
		near(t, "cpu p50", d.CPUTimeMs.P50, 50)
		near(t, "cpu p95", d.CPUTimeMs.P95, 95)
		near(t, "cpu p99", d.CPUTimeMs.P99, 99)

		// Nothing reported: sample size 0, no percentiles, runs still counted.
		old := got["deploy/1.0.0"]
		if old.InvocationCount != 3 || old.PeakRSSBytes.ReportedCount != 0 || old.CPUTimeMs.ReportedCount != 0 {
			t.Errorf("deploy 1.0.0 = %+v, want 3 invocations, 0 reported", old)
		}
		if old.PeakRSSBytes.P50 != nil || old.CPUTimeMs.P99 != nil {
			t.Errorf("deploy 1.0.0 should have no percentiles, got %+v", old)
		}

		// The two measures are tracked independently.
		s := got["status/1.1.0"]
		if s.PeakRSSBytes.ReportedCount != 5 || s.CPUTimeMs.ReportedCount != 0 || s.CPUTimeMs.P50 != nil || s.PeakRSSBytes.P50 == nil {
			t.Errorf("status 1.1.0 = %+v, want rss reported 5 with percentiles, cpu reported 0 without", s)
		}
	})

	t.Run("bucket=hour keeps the two hours apart", func(t *testing.T) {
		points, err := qb.ResourceUsage(ctx, scope, Filters{Time: rng, CommandNames: []string{"deploy"}, CLIVersions: []string{"1.1.0"}}, BucketHour)
		if err != nil {
			t.Fatalf("ResourceUsage: %v", err)
		}
		if len(points) != 2 {
			t.Fatalf("got %d points, want 2 (10:00 and 11:00): %+v", len(points), points)
		}
		var reported int64
		for _, p := range points {
			reported += p.PeakRSSBytes.ReportedCount
		}
		if reported != 100 {
			t.Errorf("reported across hours = %d, want 100", reported)
		}
	})

	t.Run("filters narrow the result", func(t *testing.T) {
		points, err := qb.ResourceUsage(ctx, scope, Filters{Time: rng, CLIVersions: []string{"1.0.0"}}, BucketDay)
		if err != nil {
			t.Fatalf("ResourceUsage: %v", err)
		}
		if len(points) != 1 || points[0].CommandName != "deploy" {
			t.Fatalf("got %+v, want only deploy 1.0.0", points)
		}
	})

	t.Run("other tenant's data is never returned", func(t *testing.T) {
		points, err := qb.ResourceUsage(ctx, scope, Filters{Time: rng, CommandNames: []string{"leak"}}, BucketDay)
		if err != nil {
			t.Fatalf("ResourceUsage: %v", err)
		}
		if len(points) != 0 {
			t.Fatalf("leaked %d points from another tenant", len(points))
		}
	})

	t.Run("unsupported bucket is rejected", func(t *testing.T) {
		if _, err := qb.ResourceUsage(ctx, scope, Filters{Time: rng}, Bucket("minute")); err == nil {
			t.Fatal("expected error for unsupported bucket")
		}
	})
}
