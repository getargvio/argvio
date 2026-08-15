package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestQueryBuilder_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live query builder test")
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

	tenantA := uuid.New()
	tenantB := uuid.New()
	for _, id := range []uuid.UUID{tenantA, tenantB} {
		if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, id, id.String(), "t"); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}

	w := NewWriter(pool)
	now := time.Now()

	// 300 spans for tenant A (mix of errors), 5 for tenant B, so a
	// tenant-A-scoped query must never see B's rows.
	var rowsA []TraceRow
	for i := 0; i < 300; i++ {
		exitCode := int32(0)
		if i%10 == 0 {
			exitCode = 1
		}
		rowsA = append(rowsA, TraceRow{
			Time: now.Add(-time.Duration(i) * time.Minute), TenantID: tenantA,
			TraceID: uuid.NewString(), SpanID: uuid.NewString(), SpanName: "cli.command.deploy",
			StartTime: now, EndTime: now.Add(120 * time.Millisecond), DurationMs: float64(100 + i%50),
			StatusCode: 1, CommandName: strp("deploy"), ExitCode: &exitCode, ConsentTier: "anonymous",
			CLIVersion: strp("1.0.0"), OS: strp("linux"),
		})
	}
	if _, err := w.InsertTraces(ctx, rowsA); err != nil {
		t.Fatalf("insert tenant A traces: %v", err)
	}

	var rowsB []TraceRow
	for i := 0; i < 5; i++ {
		rowsB = append(rowsB, TraceRow{
			Time: now, TenantID: tenantB,
			TraceID: uuid.NewString(), SpanID: uuid.NewString(), SpanName: "cli.command.deploy",
			StartTime: now, EndTime: now.Add(50 * time.Millisecond), DurationMs: 999999, // sentinel: must never appear in A's results
			StatusCode: 1, CommandName: strp("deploy"), ExitCode: i32p(0), ConsentTier: "anonymous",
		})
	}
	if _, err := w.InsertTraces(ctx, rowsB); err != nil {
		t.Fatalf("insert tenant B traces: %v", err)
	}

	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('cagg_command_stats_hourly', NULL, NULL)`); err != nil {
		t.Fatalf("refresh hourly cagg: %v", err)
	}
	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('cagg_command_stats_daily', NULL, NULL)`); err != nil {
		t.Fatalf("refresh daily cagg: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scopeA, err := NewTenantScope(tenantA)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}

	t.Run("ListTraces is tenant-scoped", func(t *testing.T) {
		results, err := qb.ListTraces(ctx, scopeA, Filters{
			Time:  TimeRange{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)},
			Limit: 1000,
		})
		if err != nil {
			t.Fatalf("ListTraces: %v", err)
		}
		if len(results) != 300 {
			t.Fatalf("got %d rows, want 300 (tenant A's own count)", len(results))
		}
		for _, r := range results {
			if r.DurationMs == 999999 {
				t.Fatalf("tenant A's query leaked tenant B's sentinel row")
			}
		}
	})

	t.Run("ListTraces respects exit_code filter", func(t *testing.T) {
		one := int32(1)
		results, err := qb.ListTraces(ctx, scopeA, Filters{
			Time:     TimeRange{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)},
			ExitCode: &one,
			Limit:    1000,
		})
		if err != nil {
			t.Fatalf("ListTraces: %v", err)
		}
		if len(results) != 30 { // i%10==0 for i in [0,300) => 30 rows
			t.Fatalf("got %d error rows, want 30", len(results))
		}
	})

	t.Run("LatencyPercentiles returns sane p50/p95/p99", func(t *testing.T) {
		points, err := qb.LatencyPercentiles(ctx, scopeA, Filters{
			Time: TimeRange{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)},
		}, BucketDay)
		if err != nil {
			t.Fatalf("LatencyPercentiles: %v", err)
		}
		if len(points) == 0 {
			t.Fatalf("expected at least one latency point")
		}
		var total int64
		for _, p := range points {
			total += p.InvocationCount
			if p.P50 == nil || p.P95 == nil || p.P99 == nil {
				t.Fatalf("expected non-nil percentiles, got %+v", p)
			}
			if *p.P50 > *p.P95 || *p.P95 > *p.P99 {
				t.Errorf("percentiles not ordered: p50=%v p95=%v p99=%v", *p.P50, *p.P95, *p.P99)
			}
		}
		if total != 300 {
			t.Errorf("summed invocation_count across buckets = %d, want 300", total)
		}
	})

	t.Run("ErrorRateSeries matches known 10%% error rate", func(t *testing.T) {
		points, err := qb.ErrorRateSeries(ctx, scopeA, Filters{
			Time: TimeRange{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)},
		}, BucketDay)
		if err != nil {
			t.Fatalf("ErrorRateSeries: %v", err)
		}
		var totalInv, totalErr int64
		for _, p := range points {
			totalInv += p.InvocationCount
			totalErr += p.ErrorCount
		}
		if totalInv != 300 || totalErr != 30 {
			t.Fatalf("invocations=%d errors=%d, want 300/30", totalInv, totalErr)
		}
	})

	t.Run("CommandFrequency never sees tenant B", func(t *testing.T) {
		points, err := qb.CommandFrequency(ctx, scopeA, Filters{
			Time: TimeRange{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)},
		}, BucketDay)
		if err != nil {
			t.Fatalf("CommandFrequency: %v", err)
		}
		var total int64
		for _, p := range points {
			total += p.InvocationCount
		}
		if total != 300 {
			t.Fatalf("total invocation count = %d, want 300 (tenant B's 5 must not leak in)", total)
		}
	})

	t.Run("attribute filter is parameterized, not injectable", func(t *testing.T) {
		// A value containing SQL metacharacters must be treated as a literal
		// string match, not executed. Since no row has this attribute, we
		// expect zero results and, critically, no error/injection.
		_, err := qb.ListTraces(ctx, scopeA, Filters{
			Time:  TimeRange{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)},
			Attrs: map[string]string{"cli.ci_provider": "'; DROP TABLE traces; --"},
			Limit: 10,
		})
		if err != nil {
			t.Fatalf("ListTraces with adversarial attribute value: %v", err)
		}
		var stillExists int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'traces'`).Scan(&stillExists); err != nil {
			t.Fatalf("check traces table survives: %v", err)
		}
		if stillExists != 1 {
			t.Fatalf("traces table was dropped — injection succeeded!")
		}
	})
}
