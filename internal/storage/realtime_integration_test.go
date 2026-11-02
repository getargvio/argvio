package storage

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every other integration test refreshes the continuous aggregates over their
// whole range first, which materializes the open bucket. A deployment never
// does, so these tests read the aggregates the way production does.

// openLiveDB migrates a throwaway database next to ARGVIO_TEST_DSN's and
// returns a pool on it. The other integration tests refresh the aggregates
// over their whole range on the shared database, which moves each watermark
// to the end of the current bucket and would hide exactly what these tests
// look for. ARGVIO_TEST_DSN must be a postgres:// URL for a superuser.
func openLiveDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live real-time aggregation test")
	}
	ctx := context.Background()

	base, err := url.Parse(dsn)
	if err != nil || (base.Scheme != "postgres" && base.Scheme != "postgresql") {
		t.Fatalf("ARGVIO_TEST_DSN must be a postgres:// URL, got %q", dsn)
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)

	name := "argvio_realtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})

	scratch := *base
	scratch.Path = "/" + name
	if err := MigrateUp(scratch.String()); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, scratch.String())
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

// A migration that recreates a continuous aggregate resets materialized_only
// to true, which hides the current bucket again (see 0024). This fails until
// that migration sets it back to false.
func TestContinuousAggregatesAreRealTime(t *testing.T) {
	ctx, pool := openLiveDB(t)

	rows, err := pool.Query(ctx, `SELECT view_name, materialized_only FROM timescaledb_information.continuous_aggregates`)
	if err != nil {
		t.Fatalf("list continuous aggregates: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		var materializedOnly bool
		if err := rows.Scan(&name, &materializedOnly); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if materializedOnly {
			t.Errorf("%s has materialized_only = true: the open bucket is invisible until the next refresh", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n == 0 {
		t.Fatal("no continuous aggregates found")
	}
}

func TestCurrentBucketIsReadableWithoutRefresh(t *testing.T) {
	ctx, pool := openLiveDB(t)

	tenant := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, tenant, tenant.String(), "t"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	now := time.Now().UTC()
	w := NewWriter(pool)
	if _, err := w.InsertLogs(ctx, []LogRow{{
		Time: now, TenantID: tenant, EventName: "cli.session.started", ConsentTier: "basic",
		CLIVersion: strp("1.0.0"), OS: strp("linux"), Arch: strp("amd64"), IsCI: boolp(false),
		LogAttributes: Attrs{"cli.install_id": "install-now"},
	}}); err != nil {
		t.Fatalf("insert logs: %v", err)
	}
	if _, err := w.InsertTraces(ctx, []TraceRow{{
		Time: now, TenantID: tenant, TraceID: uuid.NewString(), SpanID: uuid.NewString(),
		SpanName: "cli.command.deploy", StartTime: now, EndTime: now.Add(100 * time.Millisecond), DurationMs: 100,
		StatusCode: 1, CommandName: strp("deploy"), ExitCode: i32p(0), CLIVersion: strp("1.0.0"),
		OS: strp("linux"), Arch: strp("amd64"), ConsentTier: "anonymous",
	}}); err != nil {
		t.Fatalf("insert traces: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	f := Filters{Time: TimeRange{From: now.Add(-48 * time.Hour), To: now.Add(time.Hour)}}

	t.Run("SessionCohort (daily)", func(t *testing.T) {
		points, err := qb.SessionCohort(ctx, scope, f, BucketDay)
		if err != nil {
			t.Fatalf("SessionCohort: %v", err)
		}
		var total int64
		for _, p := range points {
			if p.OS == nil || *p.OS != "linux" || p.Arch == nil || *p.Arch != "amd64" {
				t.Fatalf("unexpected cohort point: %+v", p)
			}
			total += p.SessionCount
		}
		if total != 1 {
			t.Fatalf("sessions = %d, want 1 from the current day", total)
		}
	})

	t.Run("ActiveInstalls (daily)", func(t *testing.T) {
		points, err := qb.ActiveInstalls(ctx, scope, f, BucketDay)
		if err != nil {
			t.Fatalf("ActiveInstalls: %v", err)
		}
		var total int64
		for _, p := range points {
			total += p.ActiveInstalls
		}
		if total != 1 {
			t.Fatalf("active installs = %d, want 1 from the current day", total)
		}
	})

	t.Run("CommandFrequency (hourly)", func(t *testing.T) {
		points, err := qb.CommandFrequency(ctx, scope, f, BucketHour)
		if err != nil {
			t.Fatalf("CommandFrequency: %v", err)
		}
		var total int64
		for _, p := range points {
			total += p.InvocationCount
		}
		if total != 1 {
			t.Fatalf("invocations = %d, want 1 from the current hour", total)
		}
	})

	t.Run("CommandFrequency (daily)", func(t *testing.T) {
		points, err := qb.CommandFrequency(ctx, scope, f, BucketDay)
		if err != nil {
			t.Fatalf("CommandFrequency: %v", err)
		}
		var total int64
		for _, p := range points {
			total += p.InvocationCount
		}
		if total != 1 {
			t.Fatalf("invocations = %d, want 1 from the current day", total)
		}
	})
}
