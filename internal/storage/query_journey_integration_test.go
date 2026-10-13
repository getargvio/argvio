package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestErrorTypesAndFriction_Live covers the two queries that read raw logs
// alongside the exit-code rollup, against a one-day dataset small enough to
// work every expected count out by hand.
func TestErrorTypesAndFriction_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live error types / friction test")
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

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	at := day.Add(10 * time.Hour)
	rng := TimeRange{From: day, To: day.AddDate(0, 0, 1)}

	var spans []TraceRow
	span := func(tenantID uuid.UUID, command string, exitCode int32, n int) {
		for i := 0; i < n; i++ {
			spans = append(spans, TraceRow{
				Time: at, TenantID: tenantID, TraceID: uuid.NewString(), SpanID: uuid.NewString(),
				SpanName: "cli.command." + command, StartTime: at, EndTime: at.Add(time.Millisecond),
				DurationMs: 1, StatusCode: 1, CommandName: strp(command), ExitCode: i32p(exitCode), ConsentTier: "anonymous",
			})
		}
	}
	span(tenant, "deploy", 0, 5)
	span(tenant, "deploy", 130, 2)
	span(tenant, "status", 0, 1)
	span(other, "leak", 130, 4)

	var logs []LogRow
	event := func(tenantID uuid.UUID, name, command, errType string, n int) {
		for i := 0; i < n; i++ {
			attrs := Attrs{}
			if errType != "" {
				attrs["cli.error.type"] = errType
			}
			logs = append(logs, LogRow{
				Time: at, TenantID: tenantID, EventName: name, CommandName: strp(command),
				ConsentTier: "full", LogAttributes: attrs,
			})
		}
	}
	event(tenant, "cli.help.viewed", "deploy", "", 3)
	event(tenant, "cli.help.viewed", "init", "", 1) // help for a command that never ran
	event(tenant, "cli.error.raised", "deploy", "network", 2)
	event(tenant, "cli.error.raised", "deploy", "timeout", 1)
	event(other, "cli.error.raised", "leak", "network", 9)

	w := NewWriter(pool)
	if _, err := w.InsertTraces(ctx, spans); err != nil {
		t.Fatalf("insert traces: %v", err)
	}
	if _, err := w.InsertLogs(ctx, logs); err != nil {
		t.Fatalf("insert logs: %v", err)
	}
	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('cagg_exit_codes_hourly', NULL, NULL)`); err != nil {
		t.Fatalf("refresh cagg: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}

	t.Run("ErrorTypes splits by category and never sees another tenant", func(t *testing.T) {
		points, err := qb.ErrorTypes(ctx, scope, Filters{Time: rng}, BucketDay)
		if err != nil {
			t.Fatalf("ErrorTypes: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			got[*p.CommandName+"/"+*p.ErrorType] = p.ErrorCount
		}
		assertCounts(t, got, map[string]int64{"deploy/network": 2, "deploy/timeout": 1})
	})

	t.Run("Friction joins invocations, help views and interrupted runs", func(t *testing.T) {
		points, err := qb.Friction(ctx, scope, Filters{Time: rng}, BucketDay)
		if err != nil {
			t.Fatalf("Friction: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			name := *p.CommandName
			got[name+"/invocations"] = p.InvocationCount
			got[name+"/help"] = p.HelpViews
			got[name+"/interrupted"] = p.InterruptedCount
		}
		assertCounts(t, got, map[string]int64{
			"deploy/invocations": 7, "deploy/help": 3, "deploy/interrupted": 2,
			"status/invocations": 1, "status/help": 0, "status/interrupted": 0,
			"init/invocations": 0, "init/help": 1, "init/interrupted": 0,
		})
	})

	t.Run("Friction honors the command filter and hour buckets", func(t *testing.T) {
		points, err := qb.Friction(ctx, scope, Filters{Time: rng, CommandNames: []string{"deploy"}}, BucketHour)
		if err != nil {
			t.Fatalf("Friction: %v", err)
		}
		if len(points) != 1 || *points[0].CommandName != "deploy" || !points[0].Bucket.Equal(at) {
			t.Fatalf("got %+v, want one deploy row in the 10:00 bucket", points)
		}
	})
}
