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

// TestInstallQueries_Live covers the install-state queries against a small
// dataset over two ISO weeks, so every expected number can be worked out by
// hand.
func TestInstallQueries_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live install query test")
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

	today := time.Now().UTC().Truncate(24 * time.Hour)
	week0 := today.AddDate(0, 0, -int((today.Weekday()+6)%7)-14)
	week1 := week0.AddDate(0, 0, 7)
	rng := TimeRange{From: week0, To: week1.AddDate(0, 0, 7)}

	type ev struct {
		tenant    uuid.UUID
		install   string
		at        time.Time
		version   string
		os        string
		completed bool
		exit      int32
	}
	min := func(t0 time.Time, m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	w0, w1 := week0.Add(10*time.Hour), week1.Add(10*time.Hour)
	events := []ev{
		// a: ran and succeeded 90s after first seen (week0), upgrades in week1.
		{tenant, "a", w0, "1.0.0", "linux", false, 0},
		{tenant, "a", w0.Add(90 * time.Second), "1.0.0", "linux", true, 0},
		{tenant, "a", w1, "1.1.0", "linux", false, 0},
		// b: ran a command that failed (week0).
		{tenant, "b", w0, "1.0.0", "linux", false, 0},
		{tenant, "b", min(w0, 5), "1.0.0", "linux", true, 1},
		// c: a session and nothing else (week0).
		{tenant, "c", w0, "1.0.0", "darwin", false, 0},
		// d: first seen in week1, succeeded after 10 minutes, then downgrades.
		{tenant, "d", w1, "1.1.0", "darwin", false, 0},
		{tenant, "d", min(w1, 10), "1.1.0", "darwin", true, 0},
		{tenant, "d", min(w1, 60), "1.0.0", "darwin", false, 0},
		// another tenant's install must never show up.
		{other, "leak", w0, "9.9.9", "windows", true, 0},
	}
	// Install methods are reported on session starts: a and b via brew, c via
	// npm, d never reports one.
	methods := map[string]string{"a": "brew", "b": "brew", "c": "npm"}
	var in []InstallEvent
	for _, e := range events {
		x := InstallEvent{TenantID: e.tenant, InstallID: e.install, At: e.at, Version: sp(e.version), OS: sp(e.os), Completed: e.completed}
		if !e.completed {
			x.InstallMeth = methods[e.install]
		}
		if e.completed {
			x.ExitCode = &e.exit
		}
		in = append(in, x)
	}
	w := NewWriter(pool)
	if err := w.TrackInstalls(ctx, in); err != nil {
		t.Fatalf("TrackInstalls: %v", err)
	}

	session := func(tenantID uuid.UUID, at time.Time, install string) LogRow {
		attrs := Attrs{}
		if install != "" {
			attrs["cli.install_id"] = install
		}
		return LogRow{Time: at, TenantID: tenantID, EventName: "cli.session.started", ConsentTier: "basic",
			CLIVersion: sp("1.0.0"), OS: sp("linux"), Arch: sp("amd64"), LogAttributes: attrs}
	}
	if _, err := w.InsertLogs(ctx, []LogRow{
		session(tenant, w0, "a"), session(tenant, w0, "b"), session(tenant, min(w0, 1), ""), session(tenant, w1, "d"),
		session(other, w0, "leak"),
	}); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	day := func(t time.Time) string { return t.Format(time.DateOnly) }

	t.Run("funnel counts each stage per first-seen week", func(t *testing.T) {
		points, err := qb.InstallFunnel(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("InstallFunnel: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			d := day(p.Bucket.UTC())
			got[d+"/installs"], got[d+"/ran"], got[d+"/ok"] = p.Installs, p.RanCommand, p.Succeeded
		}
		assertCounts(t, got, map[string]int64{
			day(week0) + "/installs": 3, day(week0) + "/ran": 2, day(week0) + "/ok": 1,
			day(week1) + "/installs": 1, day(week1) + "/ran": 1, day(week1) + "/ok": 1,
		})
	})

	t.Run("funnel filters: first version and latest os", func(t *testing.T) {
		points, err := qb.InstallFunnel(ctx, scope, Filters{Time: rng, CLIVersions: []string{"1.1.0"}}, BucketMonth)
		if err != nil {
			t.Fatalf("InstallFunnel: %v", err)
		}
		var installs int64
		for _, p := range points {
			installs += p.Installs
		}
		if installs != 1 { // only d started on 1.1.0
			t.Errorf("installs first seen on 1.1.0 = %d, want 1", installs)
		}
		points, err = qb.InstallFunnel(ctx, scope, Filters{Time: rng, OSes: []string{"darwin"}}, BucketMonth)
		if err != nil {
			t.Fatalf("InstallFunnel: %v", err)
		}
		installs = 0
		for _, p := range points {
			installs += p.Installs
		}
		if installs != 2 { // c and d
			t.Errorf("darwin installs = %d, want 2", installs)
		}
	})

	t.Run("activation reports activated share and percentiles of activated installs", func(t *testing.T) {
		points, err := qb.InstallActivation(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("InstallActivation: %v", err)
		}
		if len(points) != 2 {
			t.Fatalf("got %d points, want 2: %+v", len(points), points)
		}
		near := func(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) < 0.5 }
		if p := points[0]; p.CohortSize != 3 || p.ActivatedCount != 1 || !near(p.P50Seconds, 90) || !near(p.P90Seconds, 90) {
			t.Errorf("week0 = %+v", p)
		}
		if p := points[1]; p.CohortSize != 1 || p.ActivatedCount != 1 || !near(p.P50Seconds, 600) {
			t.Errorf("week1 = %+v", p)
		}
	})

	t.Run("activation percentiles are null when nobody activated", func(t *testing.T) {
		points, err := qb.InstallActivation(ctx, scope, Filters{Time: rng, OSes: []string{"darwin"}, CLIVersions: []string{"1.0.0"}}, BucketWeek)
		if err != nil {
			t.Fatalf("InstallActivation: %v", err)
		}
		if len(points) != 1 || points[0].ActivatedCount != 0 || points[0].P50Seconds != nil {
			t.Errorf("points = %+v, want one bucket (install c) with no activation", points)
		}
	})

	t.Run("version changes carry direction and respect filters", func(t *testing.T) {
		points, err := qb.VersionChanges(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("VersionChanges: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			got[day(p.Bucket.UTC())+"/"+p.Direction+"/"+p.FromVersion+">"+p.ToVersion] = p.Installs
		}
		assertCounts(t, got, map[string]int64{
			day(week1) + "/upgrade/1.0.0>1.1.0":   1,
			day(week1) + "/downgrade/1.1.0>1.0.0": 1,
		})
		points, err = qb.VersionChanges(ctx, scope, Filters{Time: rng, OSes: []string{"darwin"}}, BucketWeek)
		if err != nil {
			t.Fatalf("VersionChanges: %v", err)
		}
		if len(points) != 1 || points[0].Direction != "downgrade" {
			t.Errorf("darwin changes = %+v, want only d's downgrade", points)
		}
		points, err = qb.VersionChanges(ctx, scope, Filters{Time: rng, CLIVersions: []string{"1.1.0"}}, BucketWeek)
		if err != nil {
			t.Fatalf("VersionChanges: %v", err)
		}
		if len(points) != 1 || points[0].Direction != "upgrade" {
			t.Errorf("changes to 1.1.0 = %+v, want only a's upgrade", points)
		}
	})

	t.Run("coverage compares tracked sessions with all sessions", func(t *testing.T) {
		points, err := qb.InstallCoverage(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("InstallCoverage: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			d := day(p.Bucket.UTC())
			got[d+"/sessions"], got[d+"/tracked"] = p.SessionCount, p.TrackedSessions
		}
		assertCounts(t, got, map[string]int64{
			day(week0) + "/sessions": 3, day(week0) + "/tracked": 2,
			day(week1) + "/sessions": 1, day(week1) + "/tracked": 1,
		})
	})

	t.Run("install methods count each cohort by reported method", func(t *testing.T) {
		points, err := qb.InstallMethods(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("InstallMethods: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			m := "none"
			if p.Method != nil {
				m = *p.Method
			}
			got[day(p.Bucket.UTC())+"/"+m] = p.Installs
		}
		assertCounts(t, got, map[string]int64{
			day(week0) + "/brew": 2, day(week0) + "/npm": 1, day(week1) + "/none": 1,
		})
		points, err = qb.InstallMethods(ctx, scope, Filters{Time: rng, OSes: []string{"darwin"}}, BucketWeek)
		if err != nil {
			t.Fatalf("InstallMethods: %v", err)
		}
		var total int64
		for _, p := range points {
			total += p.Installs
		}
		if total != 2 { // c and d are the darwin installs; tenant isolation holds too
			t.Errorf("darwin installs = %d, want 2", total)
		}
	})

	t.Run("new vs returning splits active installs by first-seen bucket", func(t *testing.T) {
		// Week 1 activity: a is back (first seen week 0), d is in its first
		// week and starts two sessions, which must still count once.
		if _, err := w.InsertLogs(ctx, []LogRow{
			session(tenant, min(w1, 5), "a"), session(tenant, min(w1, 6), "d"),
			session(other, min(w1, 5), "leak"),
		}); err != nil {
			t.Fatalf("InsertLogs: %v", err)
		}
		points, err := qb.NewVsReturning(ctx, scope, Filters{Time: rng}, BucketWeek)
		if err != nil {
			t.Fatalf("NewVsReturning: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			d := day(p.Bucket.UTC())
			got[d+"/new"], got[d+"/returning"] = p.New, p.Returning
		}
		assertCounts(t, got, map[string]int64{
			day(week0) + "/new": 2, day(week0) + "/returning": 0,
			day(week1) + "/new": 1, day(week1) + "/returning": 1,
		})
		points, err = qb.NewVsReturning(ctx, scope, Filters{Time: rng, OSes: []string{"darwin"}}, BucketWeek)
		if err != nil {
			t.Fatalf("NewVsReturning: %v", err)
		}
		if len(points) != 0 {
			t.Errorf("no session ran on darwin, got %+v", points)
		}
	})

	t.Run("an unsupported bucket is an error, not SQL", func(t *testing.T) {
		bad := Bucket("week'; DROP TABLE installs; --")
		if _, err := qb.InstallFunnel(ctx, scope, Filters{Time: rng}, bad); err == nil {
			t.Error("expected an error for an invalid bucket")
		}
		if _, err := qb.InstallMethods(ctx, scope, Filters{Time: rng}, bad); err == nil {
			t.Error("expected an error for an invalid bucket (install methods)")
		}
		if _, err := qb.NewVsReturning(ctx, scope, Filters{Time: rng}, bad); err == nil {
			t.Error("expected an error for an invalid bucket (new vs returning)")
		}
	})
}
