package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDashboardQueries_Live covers the multi-value filters, week/month
// re-bucketing, and the exit-code / CI-split / active-installs / retention /
// dimensions queries against a small fixed dataset spread over two ISO
// weeks, so every expected count below can be worked out by hand.
func TestDashboardQueries_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live dashboard query test")
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

	tenant := uuid.New()
	other := uuid.New()
	for _, id := range []uuid.UUID{tenant, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, id, id.String(), "t"); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}

	// week0 is the Monday (UTC) two weeks before this one; week1 the Monday
	// after it. Both are fully in the past, so every row is refreshable.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	week0 := today.AddDate(0, 0, -int((today.Weekday()+6)%7)-14)
	week1 := week0.AddDate(0, 0, 7)
	rng := TimeRange{From: week0, To: week1.AddDate(0, 0, 7)}

	type span struct {
		at       time.Time
		command  string
		exitCode int32
		version  string
		os, arch string
		isCI     bool
		n        int
	}
	spans := []span{
		{week0.Add(10 * time.Hour), "deploy", 0, "1.0.0", "linux", "amd64", false, 3},
		{week0.Add(10 * time.Hour), "deploy", 2, "1.1.0", "darwin", "arm64", true, 1},
		{week0.Add(11 * time.Hour), "status", 0, "1.1.0", "linux", "arm64", true, 2},
		{week1.Add(34 * time.Hour), "deploy", 0, "1.1.0", "linux", "amd64", false, 4},
	}
	var traceRows []TraceRow
	for _, s := range spans {
		for i := 0; i < s.n; i++ {
			traceRows = append(traceRows, TraceRow{
				Time: s.at, TenantID: tenant, TraceID: uuid.NewString(), SpanID: uuid.NewString(),
				SpanName: "cli.command." + s.command, StartTime: s.at, EndTime: s.at.Add(100 * time.Millisecond),
				DurationMs: 100, StatusCode: 1, CommandName: strp(s.command), ExitCode: i32p(s.exitCode),
				CLIVersion: strp(s.version), OS: strp(s.os), Arch: strp(s.arch), IsCI: boolp(s.isCI), ConsentTier: "anonymous",
			})
		}
	}
	// Other tenant: a command/version that must never show up in tenant's results.
	traceRows = append(traceRows, TraceRow{
		Time: week0.Add(10 * time.Hour), TenantID: other, TraceID: uuid.NewString(), SpanID: uuid.NewString(),
		SpanName: "cli.command.leak", StartTime: week0, EndTime: week0, DurationMs: 1, StatusCode: 1,
		CommandName: strp("leak"), ExitCode: i32p(0), CLIVersion: strp("9.9.9"), ConsentTier: "anonymous",
	})
	w := NewWriter(pool)
	if _, err := w.InsertTraces(ctx, traceRows); err != nil {
		t.Fatalf("insert traces: %v", err)
	}

	// Installs: A active both weeks, B only week0, C first seen week1, plus
	// one anonymous-tier session with no install id.
	session := func(at time.Time, tenantID uuid.UUID, installID, osName, arch string) LogRow {
		attrs := Attrs{}
		if installID != "" {
			attrs["cli.install_id"] = installID
		}
		return LogRow{
			Time: at, TenantID: tenantID, EventName: "cli.session.started", ConsentTier: "basic",
			CLIVersion: strp("1.0.0"), OS: strp(osName), Arch: strp(arch), IsCI: boolp(false), LogAttributes: attrs,
		}
	}
	logRows := []LogRow{
		session(week0.Add(9*time.Hour), tenant, "install-a", "linux", "amd64"),
		session(week0.Add(33*time.Hour), tenant, "install-a", "linux", "amd64"),
		session(week0.Add(9*time.Hour), tenant, "install-b", "darwin", "arm64"),
		session(week0.Add(9*time.Hour), tenant, "", "linux", "amd64"),
		session(week1.Add(9*time.Hour), tenant, "install-a", "linux", "amd64"),
		session(week1.Add(9*time.Hour), tenant, "install-c", "linux", "arm64"),
		session(week0.Add(9*time.Hour), other, "install-leak", "windows", "386"),
	}
	if _, err := w.InsertLogs(ctx, logRows); err != nil {
		t.Fatalf("insert logs: %v", err)
	}

	for _, cagg := range []string{"cagg_command_stats_hourly", "cagg_command_stats_daily", "cagg_exit_codes_hourly", "cagg_session_activity_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}

	t.Run("CommandFrequency bucket=week with multi-value command filter", func(t *testing.T) {
		points, err := qb.CommandFrequency(ctx, scope, Filters{Time: rng, CommandNames: []string{"deploy", "status"}}, BucketWeek)
		if err != nil {
			t.Fatalf("CommandFrequency: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			got[p.Bucket.UTC().Format(time.DateOnly)+"/"+p.CommandName] = p.InvocationCount
		}
		want := map[string]int64{
			week0.Format(time.DateOnly) + "/deploy": 4,
			week0.Format(time.DateOnly) + "/status": 2,
			week1.Format(time.DateOnly) + "/deploy": 4,
		}
		assertCounts(t, got, want)
	})

	t.Run("multi-value filters OR within a dimension and AND across", func(t *testing.T) {
		points, err := qb.CommandFrequency(ctx, scope, Filters{
			Time: rng, OSes: []string{"linux", "darwin"}, Arches: []string{"arm64"},
		}, BucketMonth)
		if err != nil {
			t.Fatalf("CommandFrequency: %v", err)
		}
		var total int64
		for _, p := range points {
			total += p.InvocationCount
			if p.Bucket.UTC().Day() != 1 || p.Bucket.UTC().Hour() != 0 {
				t.Errorf("month bucket %s is not the first of a month (UTC)", p.Bucket)
			}
		}
		if total != 3 { // the darwin/arm64 deploy + the two linux/arm64 status runs
			t.Fatalf("arm64 invocations on linux|darwin = %d, want 3", total)
		}
	})

	t.Run("LatencyPercentiles rolls percentile sketches up to week", func(t *testing.T) {
		points, err := qb.LatencyPercentiles(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("LatencyPercentiles: %v", err)
		}
		if len(points) != 3 {
			t.Fatalf("got %d week/command points, want 3", len(points))
		}
		for _, p := range points {
			if p.P50 == nil || *p.P50 < 90 || *p.P50 > 110 {
				t.Errorf("p50 for %s/%s = %v, want ~100", p.Bucket, p.CommandName, p.P50)
			}
		}
	})

	t.Run("ExitCodeDistribution counts per exit code", func(t *testing.T) {
		points, err := qb.ExitCodeDistribution(ctx, scope, Filters{Time: rng}, BucketDay)
		if err != nil {
			t.Fatalf("ExitCodeDistribution: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			if p.ExitCode == nil {
				t.Fatalf("unexpected nil exit code: %+v", p)
			}
			got[p.CommandName+"/"+string(rune('0'+*p.ExitCode))] += p.InvocationCount
		}
		assertCounts(t, got, map[string]int64{"deploy/0": 7, "deploy/2": 1, "status/0": 2})
	})

	t.Run("CISplit groups by is_ci in one query", func(t *testing.T) {
		points, err := qb.CISplit(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("CISplit: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			if p.IsCI == nil {
				t.Fatalf("unexpected nil is_ci: %+v", p)
			}
			key := p.Bucket.UTC().Format(time.DateOnly) + "/interactive"
			if *p.IsCI {
				key = p.Bucket.UTC().Format(time.DateOnly) + "/ci"
			}
			got[key] = p.InvocationCount
		}
		assertCounts(t, got, map[string]int64{
			week0.Format(time.DateOnly) + "/interactive": 3,
			week0.Format(time.DateOnly) + "/ci":          3,
			week1.Format(time.DateOnly) + "/interactive": 4,
		})
	})

	t.Run("SessionCohort carries and filters by arch", func(t *testing.T) {
		points, err := qb.SessionCohort(ctx, scope, Filters{Time: rng, Arches: []string{"arm64"}}, BucketWeek)
		if err != nil {
			t.Fatalf("SessionCohort: %v", err)
		}
		var total int64
		for _, p := range points {
			if p.Arch == nil || *p.Arch != "arm64" {
				t.Fatalf("arch filter leaked a non-arm64 row: %+v", p)
			}
			total += p.SessionCount
		}
		if total != 2 { // install-b in week0, install-c in week1
			t.Fatalf("arm64 sessions = %d, want 2", total)
		}
	})

	t.Run("SessionCohort rejects bucket=hour", func(t *testing.T) {
		if _, err := qb.SessionCohort(ctx, scope, Filters{Time: rng}, BucketHour); err == nil {
			t.Fatalf("expected an error for an hourly bucket on a daily rollup")
		}
	})

	t.Run("ActiveInstalls counts distinct installs per week", func(t *testing.T) {
		points, err := qb.ActiveInstalls(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("ActiveInstalls: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			got[p.Bucket.UTC().Format(time.DateOnly)] = p.ActiveInstalls
		}
		// install-a has two week0 sessions on different days — a distinct
		// count must still count it once; the anonymous session not at all.
		assertCounts(t, got, map[string]int64{week0.Format(time.DateOnly): 2, week1.Format(time.DateOnly): 2})
	})

	t.Run("Retention builds a weekly cohort grid", func(t *testing.T) {
		points, err := qb.Retention(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("Retention: %v", err)
		}
		type cell struct{ size, retained int64 }
		got := map[string]cell{}
		for _, p := range points {
			got[p.Cohort.UTC().Format(time.DateOnly)+"/"+string(rune('0'+p.Period))] = cell{p.CohortSize, p.RetainedInstalls}
		}
		want := map[string]cell{
			week0.Format(time.DateOnly) + "/0": {2, 2}, // a, b
			week0.Format(time.DateOnly) + "/1": {2, 1}, // a came back
			week1.Format(time.DateOnly) + "/0": {1, 1}, // c
		}
		if len(got) != len(want) {
			t.Fatalf("got cells %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("cell %s = %+v, want %+v", k, got[k], v)
			}
		}
	})

	t.Run("ListDimensions is tenant-scoped and time-independent", func(t *testing.T) {
		dims, err := qb.ListDimensions(ctx, scope, 100)
		if err != nil {
			t.Fatalf("ListDimensions: %v", err)
		}
		assertStrings(t, "cli_version", dims.CLIVersions, []string{"1.0.0", "1.1.0"})
		assertStrings(t, "os", dims.OSes, []string{"darwin", "linux"})
		assertStrings(t, "arch", dims.Arches, []string{"amd64", "arm64"})
		assertStrings(t, "command_name", dims.CommandNames, []string{"deploy", "status"})
	})
}

func assertCounts(t *testing.T, got, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

func assertStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
}
