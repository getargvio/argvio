package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSessionEnvironment_Live seeds session starts whose attributes can be
// counted by hand and checks each dimension's distribution.
func TestSessionEnvironment_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live session environment query test")
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
	rng := TimeRange{From: day, To: day.AddDate(0, 0, 1)}

	var logs []LogRow
	n := 0
	// session adds a cli.session.started. A nil attrs map is an anonymous-tier
	// session, which carries none of the environment attributes.
	session := func(tenantID uuid.UUID, goos string, attrs Attrs) {
		n++
		tier := "basic"
		if attrs == nil {
			tier, attrs = "anonymous", Attrs{}
		}
		logs = append(logs, LogRow{
			Time: day.Add(time.Duration(n) * time.Minute), TenantID: tenantID, EventName: "cli.session.started",
			ConsentTier: tier, CLIVersion: strp("1.0.0"), OS: strp(goos), Arch: strp("amd64"), LogAttributes: attrs,
		})
	}
	s := func(shell, terminal string) Attrs { return Attrs{"cli.shell": shell, "cli.terminal": terminal} }

	// Shells: bash 3, zsh 2, fish 1, nu 1, plus one empty string (not
	// reported) and one anonymous session.
	for range 3 {
		session(tenant, "linux", s("bash", "xterm"))
	}
	session(tenant, "darwin", s("zsh", "iTerm2"))
	session(tenant, "darwin", s("zsh", "iTerm2"))
	session(tenant, "linux", s("fish", "xterm"))
	session(tenant, "linux", s("nu", "kitty"))
	session(tenant, "linux", s("", ""))
	session(tenant, "linux", nil)

	// TTY, runtime and auth, on top of the above.
	session(tenant, "linux", Attrs{"cli.stdout.is_tty": true, "cli.runtime.name": "go", "cli.runtime.version": "1.22.1", "cli.auth.state": "authenticated"})
	session(tenant, "linux", Attrs{"cli.stdout.is_tty": true, "cli.runtime.name": "go", "cli.runtime.version": "1.22.1", "cli.auth.state": "anonymous"})
	session(tenant, "linux", Attrs{"cli.stdout.is_tty": false, "cli.runtime.name": "go", "cli.runtime.version": "1.23.0", "cli.auth.state": "unknown"})
	session(tenant, "linux", Attrs{"cli.stdout.is_tty": false, "cli.runtime.name": "node", "cli.runtime.version": "20.11.0"})
	// A version without its runtime, or a runtime without its version.
	session(tenant, "linux", Attrs{"cli.runtime.version": "9.9.9"})
	session(tenant, "linux", Attrs{"cli.runtime.name": "deno"})

	// Config customization: 0, 1, 2, 3, 5, 6, 40 and a negative count, which is
	// not a believable measurement.
	for _, c := range []int64{0, 1, 2, 3, 5, 6, 40, -4} {
		session(tenant, "linux", Attrs{"cli.config.customized_count": c})
	}
	session(tenant, "linux", Attrs{"cli.config.customized_count": 2})

	// Another tenant, and a command event, must never show up.
	session(other, "linux", Attrs{"cli.shell": "leak", "cli.stdout.is_tty": true})
	logs = append(logs, LogRow{
		Time: day.Add(time.Hour), TenantID: tenant, EventName: "cli.command.completed", ConsentTier: "basic",
		LogAttributes: Attrs{"cli.shell": "elvish"},
	})

	if _, err := NewWriter(pool).InsertLogs(ctx, logs); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	get := func(t *testing.T, dim EnvironmentDimension, f Filters) EnvironmentDistribution {
		t.Helper()
		f.Time = rng
		d, err := qb.SessionEnvironment(ctx, scope, f, dim)
		if err != nil {
			t.Fatalf("SessionEnvironment(%s): %v", dim, err)
		}
		return d
	}
	counts := func(d EnvironmentDistribution) map[string]int64 {
		m := map[string]int64{}
		for _, v := range d.Values {
			m[v.Value] = v.SessionCount
		}
		return m
	}
	check := func(t *testing.T, d EnvironmentDistribution, want map[string]int64) {
		t.Helper()
		got := counts(d)
		if len(got) != len(want) {
			t.Errorf("%s values = %v, want %v", d.Dimension, got, want)
			return
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s values = %v, want %v", d.Dimension, got, want)
				return
			}
		}
	}

	total := int64(n - 1) // every session start of the tenant, but not the other's
	t.Run("shell counts, ranked, with unreported sessions accounted for", func(t *testing.T) {
		d := get(t, EnvShell, Filters{})
		check(t, d, map[string]int64{"bash": 3, "zsh": 2, "fish": 1, "nu": 1})
		if d.SessionCount != total || d.ReportedSessions != 7 {
			t.Errorf("session_count, reported = %d, %d; want %d, 7", d.SessionCount, d.ReportedSessions, total)
		}
		if d.Other.ValueCount != 0 || d.Other.SessionCount != 0 {
			t.Errorf("other = %+v, want empty", d.Other)
		}
		if d.Values[0].Value != "bash" || d.Values[1].Value != "zsh" || d.Values[2].Value != "fish" || d.Values[3].Value != "nu" {
			t.Errorf("order = %v, want most sessions first, ties by value", d.Values)
		}
	})

	t.Run("the rest folds into other", func(t *testing.T) {
		d := get(t, EnvShell, Filters{Limit: 2})
		check(t, d, map[string]int64{"bash": 3, "zsh": 2})
		if d.Other.ValueCount != 2 || d.Other.SessionCount != 2 {
			t.Errorf("other = %+v, want 2 values, 2 sessions", d.Other)
		}
		var sum int64
		for _, v := range d.Values {
			sum += v.SessionCount
		}
		if sum+d.Other.SessionCount != d.ReportedSessions {
			t.Errorf("values %d + other %d != reported %d", sum, d.Other.SessionCount, d.ReportedSessions)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		check(t, get(t, EnvTerminal, Filters{}), map[string]int64{"xterm": 4, "iTerm2": 2, "kitty": 1})
	})

	t.Run("tty", func(t *testing.T) {
		d := get(t, EnvStdoutTTY, Filters{})
		check(t, d, map[string]int64{EnvTTY: 2, EnvPiped: 2})
	})

	t.Run("runtime", func(t *testing.T) {
		check(t, get(t, EnvRuntime, Filters{}), map[string]int64{"go": 3, "node": 1, "deno": 1})
	})

	t.Run("runtime version needs the runtime", func(t *testing.T) {
		d := get(t, EnvRuntimeVersion, Filters{})
		check(t, d, map[string]int64{"go 1.22.1": 2, "go 1.23.0": 1, "node 20.11.0": 1})
		if d.ReportedSessions != 4 {
			t.Errorf("reported = %d, want 4", d.ReportedSessions)
		}
	})

	t.Run("config customization is bucketed and never raw", func(t *testing.T) {
		d := get(t, EnvConfigCustomization, Filters{})
		check(t, d, map[string]int64{"0": 1, "1-2": 3, "3-5": 2, "6+": 2})
		if d.ReportedSessions != 8 {
			t.Errorf("reported = %d, want 8: the negative count is not reported", d.ReportedSessions)
		}
	})

	t.Run("auth state", func(t *testing.T) {
		check(t, get(t, EnvAuthState, Filters{}), map[string]int64{"authenticated": 1, "anonymous": 1, "unknown": 1})
	})

	t.Run("filters pick the sessions", func(t *testing.T) {
		d := get(t, EnvShell, Filters{OSes: []string{"darwin"}})
		check(t, d, map[string]int64{"zsh": 2})
		if d.SessionCount != 2 {
			t.Errorf("session_count = %d, want 2", d.SessionCount)
		}
	})

	t.Run("no sessions", func(t *testing.T) {
		d, err := qb.SessionEnvironment(ctx, scope, Filters{Time: TimeRange{From: day.AddDate(-1, 0, 0), To: day.AddDate(-1, 0, 1)}}, EnvShell)
		if err != nil {
			t.Fatalf("SessionEnvironment: %v", err)
		}
		if d.SessionCount != 0 || d.ReportedSessions != 0 || d.Values == nil || len(d.Values) != 0 || d.Other != (EnvironmentOther{}) {
			t.Errorf("got %+v, want an empty distribution with an empty, non-nil values list", d)
		}
	})

	t.Run("unknown dimension", func(t *testing.T) {
		if _, err := qb.SessionEnvironment(ctx, scope, Filters{Time: rng}, EnvironmentDimension("hostname")); err == nil {
			t.Error("want an error")
		}
	})
}
