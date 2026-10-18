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

// TestFeatureDiscovery_Live covers first-use tracking and the discovery query
// against a small dataset whose expected numbers can be worked out by hand.
func TestFeatureDiscovery_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live feature discovery test")
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
	w := NewWriter(pool)
	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	week0 := today.AddDate(0, 0, -int((today.Weekday()+6)%7)-30)
	week1 := week0.AddDate(0, 0, 7)
	rng := TimeRange{From: week0, To: week1.AddDate(0, 0, 7)}
	w0, w1 := week0.Add(10*time.Hour), week1.Add(10*time.Hour)
	after := func(t0 time.Time, d time.Duration) time.Time { return t0.Add(d) }
	const day = 24 * time.Hour

	// Installs: a, b, c and e are first seen in week0 (c never uses anything),
	// d in week1. d is on 1.1.0, the rest on 1.0.0.
	install := func(tenantID uuid.UUID, id string, at time.Time, version string) InstallEvent {
		return InstallEvent{TenantID: tenantID, InstallID: id, At: at, Version: sp(version)}
	}
	if err := w.TrackInstalls(ctx, []InstallEvent{
		install(tenant, "a", w0, "1.0.0"), install(tenant, "b", w0, "1.0.0"), install(tenant, "c", w0, "1.0.0"),
		install(tenant, "e", after(w0, time.Hour), "1.0.0"), install(tenant, "d", w1, "1.1.0"),
		install(other, "leak", w0, "1.0.0"),
	}); err != nil {
		t.Fatalf("TrackInstalls: %v", err)
	}

	disc := func(tenantID uuid.UUID, id, kind, name string, at time.Time) DiscoveryEvent {
		return DiscoveryEvent{TenantID: tenantID, InstallID: id, Kind: kind, Name: name, At: at}
	}
	if err := w.TrackDiscoveries(ctx, []DiscoveryEvent{
		// a: uses deploy twice in one batch (the earlier counts), and a flag later.
		disc(tenant, "a", DiscoveryFeature, "deploy", after(w0, 2*day)),
		disc(tenant, "a", DiscoveryFeature, "deploy", after(w0, 10*time.Minute)),
		disc(tenant, "a", DiscoveryFlag, "--force", after(w0, 3*day)),
		// b: finds deploy late, and the flag at exactly one day.
		disc(tenant, "b", DiscoveryFeature, "deploy", after(w0, 10*day)),
		disc(tenant, "b", DiscoveryFlag, "--force", after(w0, day)),
		// d, in the next cohort.
		disc(tenant, "d", DiscoveryFeature, "deploy", after(w1, time.Hour)),
		disc(tenant, "d", DiscoveryFeature, "init", after(w1, time.Hour)),
		// e: first use stamped before first_seen (a late session start).
		disc(tenant, "e", DiscoveryFeature, "early", w0),
		// No installs row (its session start never arrived): not in any cohort.
		disc(tenant, "ghost", DiscoveryFeature, "deploy", w0),
		disc(other, "leak", DiscoveryFeature, "secret", after(w0, time.Minute)),
	}); err != nil {
		t.Fatalf("TrackDiscoveries: %v", err)
	}
	// Later batches: a repeat activation and a replay change nothing, an
	// earlier one moves first_at back.
	if err := w.TrackDiscoveries(ctx, []DiscoveryEvent{
		disc(tenant, "a", DiscoveryFeature, "deploy", after(w0, 6*day)),
		disc(tenant, "a", DiscoveryFlag, "--force", after(w0, 3*day)),
	}); err != nil {
		t.Fatalf("TrackDiscoveries repeat: %v", err)
	}
	firstAt := func(tenantID uuid.UUID, id, kind, name string) time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, `SELECT first_at FROM install_discoveries
			WHERE tenant_id=$1 AND install_id=$2 AND kind=$3 AND name=$4`, tenantID, id, kind, name).Scan(&at); err != nil {
			t.Fatalf("read discovery %s/%s: %v", id, name, err)
		}
		return at
	}
	if got := firstAt(tenant, "a", DiscoveryFeature, "deploy"); !got.Equal(after(w0, 10*time.Minute)) {
		t.Errorf("a/deploy first_at = %v, want 10 minutes after install (repeats must not move it)", got)
	}
	if err := w.TrackDiscoveries(ctx, []DiscoveryEvent{disc(tenant, "a", DiscoveryFeature, "deploy", after(w0, 5*time.Minute))}); err != nil {
		t.Fatalf("TrackDiscoveries late: %v", err)
	}
	if got := firstAt(tenant, "a", DiscoveryFeature, "deploy"); !got.Equal(after(w0, 5*time.Minute)) {
		t.Errorf("a/deploy first_at = %v, want a late earlier event to win", got)
	}

	type row struct {
		kind, name                    string
		cohort, installs, d1, d7, d30 int64
		median                        float64
	}
	flatten := func(ps []DiscoveryPoint) []row {
		var out []row
		for _, p := range ps {
			out = append(out, row{p.Kind, p.Name, p.CohortSize, p.Installs, p.Within1Day, p.Within7Days, p.Within30Days, p.MedianSeconds})
		}
		return out
	}
	check := func(t *testing.T, got []DiscoveryPoint, want []row) {
		t.Helper()
		g := flatten(got)
		if len(g) != len(want) {
			t.Fatalf("got %+v, want %+v", g, want)
		}
		for i := range want {
			a, b := g[i], want[i]
			a.median, b.median = 0, 0
			if a != b || math.Abs(g[i].median-want[i].median) > 0.5 {
				t.Errorf("row %d = %+v, want %+v", i, g[i], want[i])
			}
		}
	}
	query := func(t *testing.T, f Filters, kind string) []DiscoveryPoint {
		t.Helper()
		ps, err := qb.FeatureDiscovery(ctx, scope, f, kind)
		if err != nil {
			t.Fatalf("FeatureDiscovery: %v", err)
		}
		return ps
	}

	// deploy: a at 5m, d at 1h, b at 10d -> median 1h. --force: b at 1d, a at 3d.
	allRows := []row{
		{DiscoveryFeature, "deploy", 5, 3, 2, 2, 3, 3600},
		{DiscoveryFeature, "early", 5, 1, 1, 1, 1, 0},
		{DiscoveryFeature, "init", 5, 1, 1, 1, 1, 3600},
		{DiscoveryFlag, "--force", 5, 2, 1, 2, 2, 172800},
	}

	t.Run("first use per install, ranked by installs within each kind", func(t *testing.T) {
		check(t, query(t, Filters{Time: rng}, ""), allRows)
	})

	t.Run("installs that never used anything stay in the cohort only", func(t *testing.T) {
		// c has no discovery and "ghost" has no install: cohort of 5 (a-e),
		// but neither appears in any row's installs.
		for _, p := range query(t, Filters{Time: rng}, "") {
			if p.CohortSize != 5 {
				t.Errorf("%s cohort size = %d, want 5", p.Name, p.CohortSize)
			}
		}
	})

	t.Run("kind narrows the result", func(t *testing.T) {
		check(t, query(t, Filters{Time: rng}, DiscoveryFlag), allRows[3:])
		check(t, query(t, Filters{Time: rng}, DiscoveryFeature), allRows[:3])
	})

	t.Run("limit and offset apply per kind", func(t *testing.T) {
		check(t, query(t, Filters{Time: rng, Limit: 1}, ""), []row{allRows[0], allRows[3]})
		check(t, query(t, Filters{Time: rng, Limit: 1, Offset: 1}, DiscoveryFeature), allRows[1:2])
	})

	t.Run("the cohort is the installs first seen in the window", func(t *testing.T) {
		got := query(t, Filters{Time: TimeRange{From: week1, To: week1.AddDate(0, 0, 7)}}, "")
		check(t, got, []row{
			{DiscoveryFeature, "deploy", 1, 1, 1, 1, 1, 3600},
			{DiscoveryFeature, "init", 1, 1, 1, 1, 1, 3600},
		})
	})

	t.Run("cli_version filters on the version first seen", func(t *testing.T) {
		got := query(t, Filters{Time: rng, CLIVersions: []string{"1.1.0"}}, DiscoveryFeature)
		check(t, got, []row{
			{DiscoveryFeature, "deploy", 1, 1, 1, 1, 1, 3600},
			{DiscoveryFeature, "init", 1, 1, 1, 1, 1, 3600},
		})
	})

	t.Run("another tenant's discoveries are never returned", func(t *testing.T) {
		for _, p := range query(t, Filters{Time: rng}, "") {
			if p.Name == "secret" {
				t.Errorf("leaked %+v", p)
			}
		}
		otherScope, _ := NewTenantScope(other)
		ps, err := qb.FeatureDiscovery(ctx, otherScope, Filters{Time: rng}, "")
		if err != nil || len(ps) != 1 || ps[0].Name != "secret" || ps[0].Installs != 1 {
			t.Errorf("other tenant sees %+v, %v; want only its own", ps, err)
		}
	})

	t.Run("an unknown kind is rejected", func(t *testing.T) {
		if _, err := qb.FeatureDiscovery(ctx, scope, Filters{Time: rng}, "command"); err == nil {
			t.Error("expected an error for kind=command")
		}
	})

	t.Run("backfill replays the earliest sighting from stored logs", func(t *testing.T) {
		bt := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, bt, bt.String(), "t"); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		now := time.Now().UTC().Truncate(time.Second)
		logRow := func(event string, at time.Time, attrs Attrs) LogRow {
			return LogRow{Time: at, TenantID: bt, EventName: event, ConsentTier: "basic", LogAttributes: attrs}
		}
		logs := []LogRow{
			logRow("cli.feature.activated", now.Add(-3*time.Hour), Attrs{"cli.install_id": "b", "cli.activation.feature": "deploy"}),
			logRow("cli.feature.activated", now.Add(-5*time.Hour), Attrs{"cli.install_id": "b", "cli.activation.feature": "deploy"}),
			logRow("cli.command.invoked", now.Add(-4*time.Hour), Attrs{"cli.install_id": "b", "cli.flag.names": []string{"--force", "-v"}}),
			logRow("cli.command.invoked", now.Add(-2*time.Hour), Attrs{"cli.install_id": "b", "cli.flag.names": []string{"--force"}}),
			logRow("cli.command.invoked", now.Add(-time.Hour), Attrs{"cli.install_id": "b"}),
			logRow("cli.feature.activated", now.Add(-time.Hour), Attrs{"cli.activation.feature": "anon"}), // anonymous tier: no install id
		}
		if _, err := w.InsertLogs(ctx, logs); err != nil {
			t.Fatalf("InsertLogs: %v", err)
		}
		since := now.Add(-24 * time.Hour)
		if _, err := BackfillDiscoveries(ctx, pool, since); err != nil {
			t.Fatalf("BackfillDiscoveries: %v", err)
		}
		if got := firstAt(bt, "b", DiscoveryFeature, "deploy"); !got.Equal(now.Add(-5 * time.Hour)) {
			t.Errorf("b/deploy first_at = %v, want the earlier activation", got)
		}
		if got := firstAt(bt, "b", DiscoveryFlag, "--force"); !got.Equal(now.Add(-4 * time.Hour)) {
			t.Errorf("b/--force first_at = %v, want the earlier invocation", got)
		}
		if got := firstAt(bt, "b", DiscoveryFlag, "-v"); !got.Equal(now.Add(-4 * time.Hour)) {
			t.Errorf("b/-v first_at = %v", got)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM install_discoveries WHERE tenant_id=$1`, bt).Scan(&n); err != nil || n != 3 {
			t.Errorf("rows for backfilled tenant = %d (%v), want 3: anonymous and flag-less events add none", n, err)
		}
		// Rerunning is harmless.
		if _, err := BackfillDiscoveries(ctx, pool, since); err != nil {
			t.Fatalf("second BackfillDiscoveries: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM install_discoveries WHERE tenant_id=$1`, bt).Scan(&n); err != nil || n != 3 {
			t.Errorf("rows after rerun = %d (%v), want 3", n, err)
		}
	})

	t.Run("sweeping an install removes its discoveries, and stale orphans age out", func(t *testing.T) {
		st := uuid.New()
		old, recent := time.Now().Add(-40*day), time.Now().Add(-2*day)
		if err := w.TrackInstalls(ctx, []InstallEvent{install(st, "old", old, "1.0.0"), install(st, "recent", recent, "1.0.0")}); err != nil {
			t.Fatalf("TrackInstalls: %v", err)
		}
		if err := w.TrackDiscoveries(ctx, []DiscoveryEvent{
			disc(st, "old", DiscoveryFeature, "deploy", old.Add(time.Hour)),
			disc(st, "recent", DiscoveryFeature, "deploy", recent.Add(time.Hour)),
			disc(st, "orphan-old", DiscoveryFeature, "deploy", old),
			disc(st, "orphan-new", DiscoveryFeature, "deploy", recent),
		}); err != nil {
			t.Fatalf("TrackDiscoveries: %v", err)
		}
		if _, err := SweepTenantInstalls(ctx, pool, st, 30); err != nil {
			t.Fatalf("SweepTenantInstalls: %v", err)
		}
		exists := func(id string) bool {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM install_discoveries WHERE tenant_id=$1 AND install_id=$2`, st, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n > 0
		}
		for id, want := range map[string]bool{"old": false, "recent": true, "orphan-old": false, "orphan-new": true} {
			if exists(id) != want {
				t.Errorf("discoveries for %s present = %v, want %v", id, !want, want)
			}
		}
	})
}
