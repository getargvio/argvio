package storage

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestVersionFootprint_Live covers the session footprint rollup: size and
// startup figures per version, the reported sample sizes, sessions that
// reported nothing, semver (not lexical) ordering with non-semver and missing
// versions deterministically last, paging over that order, filters, and
// tenant isolation.
func TestVersionFootprint_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live version footprint test")
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
	session := func(tenantID uuid.UUID, at time.Time, version *string, os string, install string, size, startup *int64) LogRow {
		attrs := Attrs{}
		if install != "" {
			attrs["cli.install_id"] = install
		}
		return LogRow{
			Time: at, TenantID: tenantID, EventName: "cli.session.started",
			CLIVersion: version, OS: strp(os), Arch: strp("amd64"), IsCI: boolp(false),
			BinarySizeBytes: size, StartupMs: startup, ConsentTier: "basic", LogAttributes: attrs,
		}
	}

	var rows []LogRow
	// 1.10.0: 100 sessions over two days from 50 installs, size 1..100 MB and
	// startup 1..100 ms, so every figure is known: median ~ 50, p95 ~ 95.
	for i := 1; i <= 100; i++ {
		at := day.Add(10*time.Hour + time.Duration(i%2)*24*time.Hour)
		rows = append(rows, session(tenant, at, strp("1.10.0"), "linux", fmt.Sprintf("inst-%d", i%50),
			i64p(int64(i)*1_000_000), i64p(int64(i))))
	}
	// 1.10.0: 20 more sessions that reported neither (a runtime with no
	// binary, or an anonymous-tier client).
	for i := 0; i < 20; i++ {
		rows = append(rows, session(tenant, day.Add(10*time.Hour), strp("1.10.0"), "linux", "", nil, nil))
	}
	// 1.9.0: size only, one install; sorts before 1.10.0 although "1.9" > "1.1".
	for i := 0; i < 5; i++ {
		rows = append(rows, session(tenant, day.Add(10*time.Hour), strp("1.9.0"), "linux", "inst-solo", i64p(9_000_000), nil))
	}
	// 2.0.0-rc.1: startup only, and a zero startup is a real measurement.
	for i := 0; i < 4; i++ {
		rows = append(rows, session(tenant, day.Add(10*time.Hour), strp("2.0.0-rc.1"), "darwin", "inst-rc", nil, i64p(0)))
	}
	// A version that isn't semver, and sessions with no version at all.
	rows = append(rows, session(tenant, day.Add(10*time.Hour), strp("nightly"), "linux", "", i64p(5), i64p(5)))
	rows = append(rows, session(tenant, day.Add(10*time.Hour), nil, "linux", "", nil, nil))
	// Another tenant: must never show up.
	rows = append(rows, session(other, day.Add(10*time.Hour), strp("9.9.9"), "linux", "", i64p(999), i64p(999)))

	if _, err := NewWriter(pool).InsertLogs(ctx, rows); err != nil {
		t.Fatalf("insert logs: %v", err)
	}
	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('cagg_session_footprint_daily', NULL, NULL)`); err != nil {
		t.Fatalf("refresh cagg: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	rng := TimeRange{From: day.Add(-time.Hour), To: day.AddDate(0, 0, 5)}
	versions := func(points []VersionFootprintPoint) []string {
		var out []string
		for _, p := range points {
			out = append(out, deref(p.CLIVersion))
		}
		return out
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
	// The HyperLogLog counts are approximate but exact at this cardinality.
	wantVersions := []string{"1.9.0", "1.10.0", "2.0.0-rc.1", "nightly", "<nil>"}

	t.Run("orders by semver, then non-semver, then missing", func(t *testing.T) {
		points, err := qb.VersionFootprint(ctx, scope, Filters{Time: rng})
		if err != nil {
			t.Fatalf("VersionFootprint: %v", err)
		}
		got := versions(points)
		if fmt.Sprint(got) != fmt.Sprint(wantVersions) {
			t.Fatalf("versions = %v, want %v", got, wantVersions)
		}
	})

	t.Run("merges days into true percentiles with sample sizes", func(t *testing.T) {
		points, err := qb.VersionFootprint(ctx, scope, Filters{Time: rng})
		if err != nil {
			t.Fatalf("VersionFootprint: %v", err)
		}
		p := points[1]
		if p.SessionCount != 120 {
			t.Errorf("session_count = %d, want 120", p.SessionCount)
		}
		if p.BinarySize.ReportedSessions != 100 || p.Startup.ReportedSessions != 100 {
			t.Errorf("reported = size %d startup %d, want 100/100", p.BinarySize.ReportedSessions, p.Startup.ReportedSessions)
		}
		if p.BinarySize.ReportedInstalls != 50 || p.Startup.ReportedInstalls != 50 {
			t.Errorf("reporting installs = size %d startup %d, want 50/50", p.BinarySize.ReportedInstalls, p.Startup.ReportedInstalls)
		}
		near(t, "size median", p.BinarySize.Median, 50_000_000)
		if p.BinarySize.Min == nil || *p.BinarySize.Min != 1_000_000 || p.BinarySize.Max == nil || *p.BinarySize.Max != 100_000_000 {
			t.Errorf("size min/max = %v/%v, want 1000000/100000000", p.BinarySize.Min, p.BinarySize.Max)
		}
		near(t, "startup p50", p.Startup.P50, 50)
		near(t, "startup p95", p.Startup.P95, 95)
	})

	t.Run("a measure nobody reported has a sample size of zero and no figures", func(t *testing.T) {
		points, err := qb.VersionFootprint(ctx, scope, Filters{Time: rng})
		if err != nil {
			t.Fatalf("VersionFootprint: %v", err)
		}
		old := points[0] // 1.9.0: size only
		if old.SessionCount != 5 || old.BinarySize.ReportedSessions != 5 || old.BinarySize.ReportedInstalls != 1 {
			t.Errorf("1.9.0 size = %+v (sessions %d), want 5 reported sessions from 1 install", old.BinarySize, old.SessionCount)
		}
		if old.Startup.ReportedSessions != 0 || old.Startup.ReportedInstalls != 0 || old.Startup.P50 != nil || old.Startup.P95 != nil {
			t.Errorf("1.9.0 startup = %+v, want nothing reported and no percentiles", old.Startup)
		}

		rc := points[2] // 2.0.0-rc.1: startup only, always zero
		if rc.BinarySize.ReportedSessions != 0 || rc.BinarySize.Median != nil || rc.BinarySize.Min != nil || rc.BinarySize.Max != nil {
			t.Errorf("2.0.0-rc.1 size = %+v, want nothing reported", rc.BinarySize)
		}
		if rc.Startup.ReportedSessions != 4 || rc.Startup.P50 == nil || *rc.Startup.P50 != 0 {
			t.Errorf("2.0.0-rc.1 startup = %+v, want 4 reported sessions at 0 ms", rc.Startup)
		}

		none := points[4] // no version at all, nothing reported
		if none.CLIVersion != nil || none.SessionCount != 1 || none.BinarySize.ReportedSessions != 0 || none.Startup.ReportedSessions != 0 {
			t.Errorf("missing version = %+v, want 1 session, nothing reported", none)
		}
	})

	t.Run("filters narrow the result", func(t *testing.T) {
		points, err := qb.VersionFootprint(ctx, scope, Filters{Time: rng, OSes: []string{"darwin"}})
		if err != nil {
			t.Fatalf("VersionFootprint: %v", err)
		}
		if got := versions(points); fmt.Sprint(got) != "[2.0.0-rc.1]" {
			t.Fatalf("versions = %v, want only 2.0.0-rc.1", got)
		}
	})

	t.Run("a window without the sessions is empty", func(t *testing.T) {
		points, err := qb.VersionFootprint(ctx, scope, Filters{Time: TimeRange{From: day.AddDate(0, 0, 10), To: day.AddDate(0, 0, 20)}})
		if err != nil {
			t.Fatalf("VersionFootprint: %v", err)
		}
		if len(points) != 0 {
			t.Fatalf("got %d points, want 0", len(points))
		}
	})

	t.Run("limit and offset page over the version order", func(t *testing.T) {
		points, err := qb.VersionFootprint(ctx, scope, Filters{Time: rng, Limit: 2, Offset: 1})
		if err != nil {
			t.Fatalf("VersionFootprint: %v", err)
		}
		if got := versions(points); fmt.Sprint(got) != fmt.Sprint(wantVersions[1:3]) {
			t.Fatalf("versions = %v, want %v", got, wantVersions[1:3])
		}
	})

	t.Run("other tenant's data is never returned", func(t *testing.T) {
		points, err := qb.VersionFootprint(ctx, scope, Filters{Time: rng, CLIVersions: []string{"9.9.9"}})
		if err != nil {
			t.Fatalf("VersionFootprint: %v", err)
		}
		if len(points) != 0 {
			t.Fatalf("leaked %d points from another tenant", len(points))
		}
	})
}
