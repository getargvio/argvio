package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestColdWarmLatency_Live seeds installs whose first runs and runs after an
// upgrade can be worked out by hand, and checks the cold and warm groups.
func TestColdWarmLatency_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live cold/warm latency query test")
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
	t0 := day.Add(10 * time.Hour)
	rng := TimeRange{From: day, To: day.AddDate(0, 0, 1)}
	min := func(n int) time.Duration { return time.Duration(n) * time.Minute }

	var logs []LogRow
	var events []InstallEvent
	// run adds a cli.command.completed of install at offset from t0. A nil
	// duration is a run that did not report one; an empty install is an
	// anonymous-tier event.
	run := func(tenantID uuid.UUID, install, command, version string, ms *int64, offset time.Duration) {
		attrs := Attrs{}
		tier := "anonymous"
		if install != "" {
			attrs["cli.install_id"] = install
			tier = "basic"
		}
		if ms != nil {
			attrs["cli.command.duration_ms"] = *ms
		}
		r := LogRow{
			Time: t0.Add(offset), TenantID: tenantID, EventName: "cli.command.completed", ConsentTier: tier,
			CLIVersion: strp(version), OS: strp("linux"), Arch: strp("amd64"),
			CommandName: strp(command), ExitCode: i32p(0), LogAttributes: attrs,
		}
		logs = append(logs, r)
		if ev, ok := InstallEventFromLog(r); ok {
			events = append(events, ev)
		}
	}
	d := func(v int64) *int64 { return &v }

	// a: first run of anything is a "build" (cold, 1000), then warm runs, then
	// upgrades to 1.1.0, whose first run is a "test" (cold, 800) even though
	// "test" was run before.
	run(tenant, "a", "build", "1.0.0", d(1000), 0)
	run(tenant, "a", "test", "1.0.0", d(100), min(1))
	run(tenant, "a", "build", "1.0.0", d(200), min(2))
	run(tenant, "a", "test", "1.1.0", d(800), min(10))
	run(tenant, "a", "test", "1.1.0", d(120), min(11))
	// b: first run is "test" (cold, 600); its second run has no duration and
	// is left out of both groups.
	run(tenant, "b", "test", "1.0.0", d(600), 0)
	run(tenant, "b", "test", "1.0.0", nil, min(1))
	run(tenant, "b", "test", "1.0.0", d(140), min(2))
	// d: one run ever, so cold with nothing warm to compare it to.
	run(tenant, "d", "lint", "1.0.0", d(300), 0)
	// c: anonymous tier, no install id: invisible.
	run(tenant, "", "build", "1.0.0", d(5000), 0)
	// another tenant's install must never show up.
	run(other, "leak", "build", "1.0.0", d(9999), 0)

	w := NewWriter(pool)
	if _, err := w.InsertLogs(ctx, logs); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}
	if err := w.TrackInstalls(ctx, events); err != nil {
		t.Fatalf("TrackInstalls: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}

	get := func(t *testing.T, f Filters) map[string]ColdWarmLatencyPoint {
		t.Helper()
		f.Time = rng
		list, err := qb.ColdWarmLatency(ctx, scope, f, BucketDay)
		if err != nil {
			t.Fatalf("ColdWarmLatency: %v", err)
		}
		m := map[string]ColdWarmLatencyPoint{}
		for _, p := range list {
			m[p.CommandName] = p
		}
		return m
	}

	t.Run("cold and warm are split per command", func(t *testing.T) {
		got := get(t, Filters{})
		if len(got) != 3 {
			t.Fatalf("got %d commands, want build, lint and test: %+v", len(got), got)
		}
		if lint := got["lint"]; lint.Cold.RunCount != 1 || lint.Warm.RunCount != 0 || lint.Warm.P50 != nil || lint.Cold.P50 == nil {
			t.Errorf("lint = %+v; want one cold run and no warm percentiles", lint)
		}
		build, test := got["build"], got["test"]
		// build: a's first run is cold; its second is warm.
		if build.Cold.RunCount != 1 || build.Warm.RunCount != 1 {
			t.Errorf("build counts = %d cold, %d warm; want 1, 1", build.Cold.RunCount, build.Warm.RunCount)
		}
		if build.Cold.P50 == nil || *build.Cold.P50 < 990 || *build.Cold.P50 > 1010 {
			t.Errorf("build cold p50 = %v, want ~1000", build.Cold.P50)
		}
		if build.Warm.P50 == nil || *build.Warm.P50 < 195 || *build.Warm.P50 > 205 {
			t.Errorf("build warm p50 = %v, want ~200", build.Warm.P50)
		}
		// test: cold are b's first run (600) and a's first run after upgrading
		// (800); warm are a's 100 and 120 and b's 140 (b's run without a
		// duration is not sampled).
		if test.Cold.RunCount != 2 || test.Warm.RunCount != 3 {
			t.Errorf("test counts = %d cold, %d warm; want 2, 3", test.Cold.RunCount, test.Warm.RunCount)
		}
		if test.Cold.P99 == nil || *test.Cold.P99 < 590 || *test.Cold.P99 > 810 {
			t.Errorf("test cold p99 = %v, want within 600..800", test.Cold.P99)
		}
	})

	t.Run("version filter", func(t *testing.T) {
		// Only the 1.1.0 runs: a's post-upgrade run is cold, its next is warm.
		got := get(t, Filters{CLIVersions: []string{"1.1.0"}})
		test := got["test"]
		if test.Cold.RunCount != 1 || test.Warm.RunCount != 1 {
			t.Errorf("1.1.0 test counts = %d cold, %d warm; want 1, 1", test.Cold.RunCount, test.Warm.RunCount)
		}
		if _, found := got["build"]; found {
			t.Errorf("build has no 1.1.0 runs, should not be listed")
		}
	})

	t.Run("filters pick the runs, not which are cold", func(t *testing.T) {
		// Filtering to test drops a's cold build run; b's first-ever run and
		// a's first post-upgrade run stay cold.
		got := get(t, Filters{CommandNames: []string{"test"}})
		if test := got["test"]; test.Cold.RunCount != 2 || test.Warm.RunCount != 3 {
			t.Errorf("test = %d cold, %d warm; want 2, 3", test.Cold.RunCount, test.Warm.RunCount)
		}
	})

	t.Run("paging", func(t *testing.T) {
		list, err := qb.ColdWarmLatency(ctx, scope, Filters{Time: rng, Limit: 1, Offset: 1}, BucketDay)
		if err != nil {
			t.Fatalf("ColdWarmLatency: %v", err)
		}
		if len(list) != 1 || list[0].CommandName != "lint" {
			t.Errorf("page 2 of 1: got %+v, want lint", list)
		}
	})

	t.Run("unsupported bucket", func(t *testing.T) {
		if _, err := qb.ColdWarmLatency(ctx, scope, Filters{Time: rng}, Bucket("year")); err == nil {
			t.Error("want an error")
		}
	})
}
