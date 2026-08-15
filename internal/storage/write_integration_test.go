package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func strp(s string) *string   { return &s }
func i32p(i int32) *int32     { return &i }
func i64p(i int64) *int64     { return &i }
func f64p(f float64) *float64 { return &f }
func boolp(b bool) *bool      { return &b }

func TestWriter_InsertAllSignals_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live writer test")
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

	tenantID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, tenantID, "writer-test", "Writer Test"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	w := NewWriter(pool)
	now := time.Now()

	traceRows := []TraceRow{{
		Time: now, TenantID: tenantID,
		TraceID: "trace-1", SpanID: "span-1", SpanName: "cli.command.deploy", SpanKind: 1,
		StartTime: now, EndTime: now.Add(200 * time.Millisecond), DurationMs: 200, StatusCode: 1,
		CLIVersion: strp("1.2.3"), OS: strp("linux"), Arch: strp("amd64"),
		CommandName: strp("deploy"), ExitCode: i32p(0), IsCI: boolp(false), ConsentTier: "anonymous",
		ResourceAttributes: Attrs{"service.name": "acme-cli"},
		SpanAttributes:     Attrs{"cli.command.name": "deploy"},
	}}
	n, err := w.InsertTraces(ctx, traceRows)
	if err != nil || n != 1 {
		t.Fatalf("InsertTraces: n=%d err=%v", n, err)
	}

	logRows := []LogRow{{
		Time: now, TenantID: tenantID, EventName: "cli.command.invoked",
		CLIVersion: strp("1.2.3"), OS: strp("linux"), Arch: strp("amd64"),
		CommandName: strp("deploy"), IsCI: boolp(false), ConsentTier: "basic",
		LogAttributes: Attrs{"cli.session_id": "sess-1"},
	}}
	n, err = w.InsertLogs(ctx, logRows)
	if err != nil || n != 1 {
		t.Fatalf("InsertLogs: n=%d err=%v", n, err)
	}

	metricRows := []MetricRow{
		{
			Time: now, TenantID: tenantID, MetricName: "cli.command.invocations", MetricType: "sum",
			Unit: strp("1"), Value: f64p(1), CLIVersion: strp("1.2.3"), OS: strp("linux"),
			CommandName: strp("deploy"), ConsentTier: "anonymous",
		},
		{
			Time: now, TenantID: tenantID, MetricName: "cli.command.duration", MetricType: "histogram",
			Unit: strp("ms"), HistogramCount: i64p(3), HistogramSum: f64p(600),
			HistogramMin: f64p(150), HistogramMax: f64p(250),
			HistogramBucketBounds: []float64{100, 200, 300},
			HistogramBucketCounts: []int64{0, 2, 1},
			CommandName:           strp("deploy"), ConsentTier: "anonymous",
		},
	}
	n, err = w.InsertMetrics(ctx, metricRows)
	if err != nil || n != 2 {
		t.Fatalf("InsertMetrics: n=%d err=%v", n, err)
	}

	var traceCount, logCount, metricCount int
	pool.QueryRow(ctx, `SELECT count(*) FROM traces WHERE tenant_id=$1`, tenantID).Scan(&traceCount)
	pool.QueryRow(ctx, `SELECT count(*) FROM logs WHERE tenant_id=$1`, tenantID).Scan(&logCount)
	pool.QueryRow(ctx, `SELECT count(*) FROM metrics WHERE tenant_id=$1`, tenantID).Scan(&metricCount)
	if traceCount != 1 || logCount != 1 || metricCount != 2 {
		t.Fatalf("row counts: traces=%d logs=%d metrics=%d", traceCount, logCount, metricCount)
	}

	var bounds []float64
	var counts []int64
	if err := pool.QueryRow(ctx, `SELECT histogram_bucket_bounds, histogram_bucket_counts FROM metrics WHERE tenant_id=$1 AND metric_type='histogram'`, tenantID).Scan(&bounds, &counts); err != nil {
		t.Fatalf("scan histogram arrays: %v", err)
	}
	if len(bounds) != 3 || len(counts) != 3 {
		t.Fatalf("histogram arrays not round-tripped: bounds=%v counts=%v", bounds, counts)
	}
}
