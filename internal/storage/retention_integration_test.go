package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSweepTenantRetention_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live retention sweep test")
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
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, tenantID, tenantID.String(), "t"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	w := NewWriter(pool)
	now := time.Now()
	rows := []TraceRow{
		{Time: now.Add(-5 * 24 * time.Hour), TenantID: tenantID, TraceID: "t1", SpanID: "s1", SpanName: "cli.command.old",
			StartTime: now, EndTime: now, DurationMs: 1, StatusCode: 1, ConsentTier: "anonymous"},
		{Time: now.Add(-1 * time.Hour), TenantID: tenantID, TraceID: "t2", SpanID: "s2", SpanName: "cli.command.new",
			StartTime: now, EndTime: now, DurationMs: 1, StatusCode: 1, ConsentTier: "anonymous"},
	}
	if _, err := w.InsertTraces(ctx, rows); err != nil {
		t.Fatalf("insert traces: %v", err)
	}

	n, err := SweepTenantRetention(ctx, pool, "traces", tenantID, 1)
	if err != nil {
		t.Fatalf("SweepTenantRetention: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d rows, want 1", n)
	}

	var remaining []string
	qrows, err := pool.Query(ctx, `SELECT span_name FROM traces WHERE tenant_id = $1`, tenantID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer qrows.Close()
	for qrows.Next() {
		var name string
		if err := qrows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		remaining = append(remaining, name)
	}
	if len(remaining) != 1 || remaining[0] != "cli.command.new" {
		t.Fatalf("remaining rows = %v, want only cli.command.new", remaining)
	}
}

func TestSweepTenantRetention_RejectsUnknownTable(t *testing.T) {
	if _, err := SweepTenantRetention(context.Background(), nil, "tenants", uuid.New(), 1); err == nil {
		t.Fatalf("expected error for non-sweepable table")
	}
}
