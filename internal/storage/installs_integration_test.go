package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInstalls_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live installs test")
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
	base := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	h := func(n int) time.Time { return base.Add(time.Duration(n) * time.Hour) }

	type installRow struct {
		firstSeen, lastSeen        time.Time
		firstVersion, lastVersion  *string
		method                     *string
		firstCommand, firstSuccess *time.Time
	}
	load := func(t *testing.T, tenantID uuid.UUID, id string) (installRow, bool) {
		t.Helper()
		var r installRow
		err := pool.QueryRow(ctx, `SELECT first_seen, last_seen, first_version, last_version, install_method,
			first_command_at, first_success_at FROM installs WHERE tenant_id=$1 AND install_id=$2`, tenantID, id).
			Scan(&r.firstSeen, &r.lastSeen, &r.firstVersion, &r.lastVersion, &r.method, &r.firstCommand, &r.firstSuccess)
		return r, err == nil
	}
	changes := func(t *testing.T, tenantID uuid.UUID, id string) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT direction || ' ' || from_version || '>' || to_version
			FROM install_version_changes WHERE tenant_id=$1 AND install_id=$2 ORDER BY at`, tenantID, id)
		if err != nil {
			t.Fatalf("query changes: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	event := func(tenantID uuid.UUID, id string, at time.Time, version string, completed bool, exit int32) InstallEvent {
		e := InstallEvent{TenantID: tenantID, InstallID: id, At: at, Version: sp(version), Completed: completed}
		if completed {
			e.ExitCode = &exit
		}
		return e
	}

	t.Run("batches accumulate state and version changes", func(t *testing.T) {
		first := event(tenant, "a", h(1), "1.0.0", false, 0)
		first.InstallMeth = "brew"
		if err := w.TrackInstalls(ctx, []InstallEvent{first, event(tenant, "a", h(2), "1.0.0", true, 1)}); err != nil {
			t.Fatalf("TrackInstalls: %v", err)
		}
		if err := w.TrackInstalls(ctx, []InstallEvent{event(tenant, "a", h(5), "1.1.0", true, 0), event(tenant, "a", h(9), "1.0.5", false, 0)}); err != nil {
			t.Fatalf("TrackInstalls: %v", err)
		}
		r, ok := load(t, tenant, "a")
		if !ok {
			t.Fatal("install a missing")
		}
		if !r.firstSeen.Equal(h(1)) || !r.lastSeen.Equal(h(9)) || *r.firstVersion != "1.0.0" || *r.lastVersion != "1.0.5" {
			t.Errorf("state = %+v", r)
		}
		if r.method == nil || *r.method != "brew" {
			t.Errorf("install method = %v", r.method)
		}
		if r.firstCommand == nil || !r.firstCommand.Equal(h(2)) || r.firstSuccess == nil || !r.firstSuccess.Equal(h(5)) {
			t.Errorf("first command/success = %v / %v", r.firstCommand, r.firstSuccess)
		}
		want := []string{"upgrade 1.0.0>1.1.0", "downgrade 1.1.0>1.0.5"}
		if got := changes(t, tenant, "a"); !equal(got, want) {
			t.Errorf("changes = %v, want %v", got, want)
		}
	})

	t.Run("replaying and late events are harmless", func(t *testing.T) {
		replay := []InstallEvent{
			event(tenant, "a", h(5), "1.1.0", true, 0),
			event(tenant, "a", h(9), "1.0.5", false, 0),
			event(tenant, "a", h(0), "0.9.0", false, 0), // arrives late
		}
		if err := w.TrackInstalls(ctx, replay); err != nil {
			t.Fatalf("TrackInstalls: %v", err)
		}
		r, _ := load(t, tenant, "a")
		if !r.firstSeen.Equal(h(0)) || *r.firstVersion != "0.9.0" || *r.lastVersion != "1.0.5" || !r.lastSeen.Equal(h(9)) {
			t.Errorf("state = %+v", r)
		}
		if got := changes(t, tenant, "a"); len(got) != 2 {
			t.Errorf("changes = %v, want the original 2", got)
		}
	})

	t.Run("tenants are isolated even with the same install id", func(t *testing.T) {
		if err := w.TrackInstalls(ctx, []InstallEvent{event(other, "a", h(30), "9.9.9", false, 0)}); err != nil {
			t.Fatalf("TrackInstalls: %v", err)
		}
		r, _ := load(t, tenant, "a")
		if *r.lastVersion != "1.0.5" {
			t.Errorf("other tenant's event changed this tenant's install: %+v", r)
		}
		o, ok := load(t, other, "a")
		if !ok || *o.firstVersion != "9.9.9" {
			t.Errorf("other tenant's install = %+v, %v", o, ok)
		}
	})

	t.Run("backfill rebuilds the same state from stored logs", func(t *testing.T) {
		bt := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, bt, bt.String(), "t"); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		row := func(name string, at time.Time, version string, exit *int32, attrs Attrs) LogRow {
			attrs["cli.install_id"] = "b"
			return LogRow{Time: at, TenantID: bt, EventName: name, CLIVersion: sp(version), ExitCode: exit, ConsentTier: "basic", LogAttributes: attrs}
		}
		zero := int32(0)
		logs := []LogRow{
			row("cli.session.started", h(1), "1.0.0", nil, Attrs{"cli.install.method": "npm"}),
			row("cli.command.completed", h(2), "1.0.0", &zero, Attrs{}),
			row("cli.session.started", h(3), "2.0.0", nil, Attrs{}),
			{Time: h(4), TenantID: bt, EventName: "cli.session.started", ConsentTier: "anonymous", LogAttributes: Attrs{}}, // no install id
			{Time: h(4), TenantID: bt, EventName: "cli.help.viewed", ConsentTier: "basic", LogAttributes: Attrs{"cli.install_id": "b"}},
		}
		if _, err := w.InsertLogs(ctx, logs); err != nil {
			t.Fatalf("InsertLogs: %v", err)
		}
		n, err := BackfillInstalls(ctx, pool, base.Add(-time.Hour))
		if err != nil {
			t.Fatalf("BackfillInstalls: %v", err)
		}
		if n < 3 {
			t.Errorf("replayed %d events, want at least 3", n)
		}
		r, ok := load(t, bt, "b")
		if !ok || !r.firstSeen.Equal(h(1)) || !r.lastSeen.Equal(h(3)) || *r.lastVersion != "2.0.0" || *r.method != "npm" {
			t.Fatalf("state = %+v, %v", r, ok)
		}
		if r.firstSuccess == nil || !r.firstSuccess.Equal(h(2)) {
			t.Errorf("first success = %v", r.firstSuccess)
		}
		if got := changes(t, bt, "b"); !equal(got, []string{"upgrade 1.0.0>2.0.0"}) {
			t.Errorf("changes = %v", got)
		}
		// Rerunning changes nothing.
		if _, err := BackfillInstalls(ctx, pool, base.Add(-time.Hour)); err != nil {
			t.Fatalf("second BackfillInstalls: %v", err)
		}
		if got := changes(t, bt, "b"); len(got) != 1 {
			t.Errorf("changes after rerun = %v", got)
		}
	})

	t.Run("sweep purges by last_seen, per tenant and by default window", func(t *testing.T) {
		st, dt, kt := uuid.New(), uuid.New(), uuid.New()
		old, recent := time.Now().Add(-40*24*time.Hour), time.Now().Add(-2*24*time.Hour)
		if err := w.TrackInstalls(ctx, []InstallEvent{
			event(st, "old", old, "1.0.0", false, 0), event(st, "recent", recent, "1.0.0", false, 0),
			event(dt, "old", old, "1.0.0", false, 0),
			event(kt, "old", old, "1.0.0", false, 0),
		}); err != nil {
			t.Fatalf("TrackInstalls: %v", err)
		}
		if err := w.TrackInstalls(ctx, []InstallEvent{event(st, "old", old.Add(time.Hour), "2.0.0", false, 0)}); err != nil {
			t.Fatalf("TrackInstalls: %v", err)
		}

		// st has a 30-day override; kt has a longer override, so the default
		// window (30d here) must leave it alone; dt uses the default.
		n, err := SweepTenantInstalls(ctx, pool, st, 30)
		if err != nil || n != 1 {
			t.Fatalf("SweepTenantInstalls = %d, %v; want 1", n, err)
		}
		// The DB may hold other tenants' installs from earlier runs, so check
		// outcomes per install rather than the total count.
		if _, err = SweepInstallsExcept(ctx, pool, 30, []uuid.UUID{st, kt, tenant, other}); err != nil {
			t.Fatalf("SweepInstallsExcept: %v", err)
		}
		if _, ok := load(t, st, "old"); ok {
			t.Error("st/old should be swept")
		}
		if _, ok := load(t, st, "recent"); !ok {
			t.Error("st/recent should survive")
		}
		if _, ok := load(t, dt, "old"); ok {
			t.Error("dt/old should be swept by the default window")
		}
		if _, ok := load(t, kt, "old"); !ok {
			t.Error("kt/old is excluded and must survive")
		}
		if got := changes(t, st, "old"); len(got) != 0 {
			t.Errorf("swept install's version changes remain: %v", got)
		}
	})
}
