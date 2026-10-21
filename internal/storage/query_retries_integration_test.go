package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRetries_Live seeds installs whose retries and failure streaks can be
// worked out by hand and checks the per-command figures against them.
func TestRetries_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live retries query test")
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

	var logs []LogRow
	// run adds one cli.command.completed of install at offset from t0; an
	// empty install is an anonymous-tier event, which carries none.
	run := func(tenantID uuid.UUID, install, command string, exit *int32, offset time.Duration) {
		attrs := Attrs{}
		tier := "anonymous"
		if install != "" {
			attrs["cli.install_id"] = install
			tier = "basic"
		}
		logs = append(logs, LogRow{
			Time: t0.Add(offset), TenantID: tenantID, EventName: "cli.command.completed", ConsentTier: tier,
			CommandName: strp(command), ExitCode: exit, LogAttributes: attrs,
		})
	}
	fail := func(install, command string, offset time.Duration) { run(tenant, install, command, i32p(1), offset) }
	ok := func(install, command string, offset time.Duration) { run(tenant, install, command, i32p(0), offset) }
	min := func(n int) time.Duration { return time.Duration(n) * time.Minute }

	// deploy: failure, then a success 5 minutes later.
	fail("a", "deploy", 0)
	ok("a", "deploy", min(5))
	// build: the window boundary. b's success is exactly 10 minutes after the
	// failure (a retry); c's is a second past it (an independent run).
	fail("b", "build", 0)
	ok("b", "build", min(10))
	fail("c", "build", 0)
	ok("c", "build", min(10)+time.Second)
	// test: d and e interleave in time and each fail three times (e never
	// recovers); f fails twice. d recovers.
	fail("d", "test", 0)
	fail("e", "test", min(1))
	fail("f", "test", 0)
	fail("d", "test", min(3))
	fail("e", "test", min(4))
	fail("f", "test", min(2))
	fail("d", "test", min(6))
	fail("e", "test", min(7))
	ok("d", "test", min(8))
	// push and pull interleave on install g: push fails and recovers, pull
	// fails twice. Neither's chain is broken by the other command.
	fail("g", "push", 0)
	fail("g", "pull", min(1))
	ok("g", "push", min(2))
	fail("g", "pull", min(3))
	// lint-chain: four failures 8 minutes apart form a streak of 4, yet no
	// three of them lie within 10 minutes. lint-burst: three failures spanning
	// exactly 10 minutes are a repeated failure.
	for i := 0; i < 4; i++ {
		fail("h", "lint-chain", min(8*i))
	}
	fail("i", "lint-burst", 0)
	fail("i", "lint-burst", min(5))
	fail("i", "lint-burst", min(10))
	// flaky: a lone failure after a success is not retried.
	ok("j", "flaky", 0)
	fail("j", "flaky", min(5))
	// fine: successes only, so not listed.
	ok("j", "fine", 0)
	ok("j", "fine", min(1))
	// Not seen: anonymous-tier failures (no install id), a completion without
	// an exit code, and another tenant's install.
	for i := 0; i < 3; i++ {
		run(tenant, "", "anon", i32p(1), min(i))
	}
	run(tenant, "k", "noexit", nil, 0)
	run(tenant, "k", "noexit", nil, min(1))
	run(other, "leak", "leak", i32p(1), 0)
	run(other, "leak", "leak", i32p(1), min(1))

	if _, err := NewWriter(pool).InsertLogs(ctx, logs); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}

	byCommand := func(t *testing.T, got []RetryPoint) map[string]RetryPoint {
		t.Helper()
		m := map[string]RetryPoint{}
		for _, p := range got {
			if p.CommandName == nil {
				t.Fatalf("row with no command name: %+v", p)
			}
			m[*p.CommandName] = p
		}
		return m
	}
	check := func(t *testing.T, got map[string]RetryPoint, command string, want RetryPoint) {
		t.Helper()
		g, found := got[command]
		if !found {
			t.Errorf("%s: not listed", command)
			return
		}
		g.CommandName, g.RetryRate = nil, 0
		if g != want {
			t.Errorf("%s = %+v, want %+v", command, g, want)
		}
	}

	t.Run("per command", func(t *testing.T) {
		list, err := qb.Retries(ctx, scope, Filters{Time: rng}, RetryWindow)
		if err != nil {
			t.Fatalf("Retries: %v", err)
		}
		got := byCommand(t, list)
		//                                         failed retried seqs recovered repeated streak
		check(t, got, "deploy", RetryPoint{FailedCount: 1, RetriedCount: 1, RetrySequences: 1, RecoveredSequences: 1, RepeatedFailureInstalls: 0, LongestFailureStreak: 1})
		check(t, got, "build", RetryPoint{FailedCount: 2, RetriedCount: 1, RetrySequences: 1, RecoveredSequences: 1, RepeatedFailureInstalls: 0, LongestFailureStreak: 1})
		check(t, got, "test", RetryPoint{FailedCount: 8, RetriedCount: 6, RetrySequences: 3, RecoveredSequences: 1, RepeatedFailureInstalls: 2, LongestFailureStreak: 3})
		check(t, got, "push", RetryPoint{FailedCount: 1, RetriedCount: 1, RetrySequences: 1, RecoveredSequences: 1, RepeatedFailureInstalls: 0, LongestFailureStreak: 1})
		check(t, got, "pull", RetryPoint{FailedCount: 2, RetriedCount: 1, RetrySequences: 1, RecoveredSequences: 0, RepeatedFailureInstalls: 0, LongestFailureStreak: 2})
		check(t, got, "lint-chain", RetryPoint{FailedCount: 4, RetriedCount: 3, RetrySequences: 1, RecoveredSequences: 0, RepeatedFailureInstalls: 0, LongestFailureStreak: 4})
		check(t, got, "lint-burst", RetryPoint{FailedCount: 3, RetriedCount: 2, RetrySequences: 1, RecoveredSequences: 0, RepeatedFailureInstalls: 1, LongestFailureStreak: 3})
		check(t, got, "flaky", RetryPoint{FailedCount: 1, RetriedCount: 0, RetrySequences: 0, RecoveredSequences: 0, RepeatedFailureInstalls: 0, LongestFailureStreak: 1})
		for _, name := range []string{"fine", "anon", "noexit", "leak"} {
			if _, found := got[name]; found {
				t.Errorf("%s should not be listed", name)
			}
		}
		if len(list) != 8 {
			t.Errorf("got %d commands, want 8", len(list))
		}
		// Most failures first, ties by command name.
		if list[0].CommandName == nil || *list[0].CommandName != "test" {
			t.Errorf("first command = %v, want test", list[0].CommandName)
		}
		if p := got["build"]; p.RetryRate != 0.5 {
			t.Errorf("build retry_rate = %v, want 0.5", p.RetryRate)
		}
	})

	t.Run("a shorter window retries less", func(t *testing.T) {
		list, err := qb.Retries(ctx, scope, Filters{Time: rng}, 2*time.Minute)
		if err != nil {
			t.Fatalf("Retries: %v", err)
		}
		got := byCommand(t, list)
		// deploy's success is 5 minutes after the failure; with 2 minutes that
		// is an independent run.
		check(t, got, "deploy", RetryPoint{FailedCount: 1, LongestFailureStreak: 1})
		// Only two links hold: f's failures are 2 minutes apart, and d's last
		// failure is 2 minutes before its success.
		check(t, got, "test", RetryPoint{FailedCount: 8, RetriedCount: 2, RetrySequences: 2, RecoveredSequences: 1, RepeatedFailureInstalls: 0, LongestFailureStreak: 2})
	})

	t.Run("command filter and paging", func(t *testing.T) {
		list, err := qb.Retries(ctx, scope, Filters{Time: rng, CommandNames: []string{"build"}}, RetryWindow)
		if err != nil {
			t.Fatalf("Retries: %v", err)
		}
		if len(list) != 1 || list[0].CommandName == nil || *list[0].CommandName != "build" {
			t.Errorf("command filter: got %+v, want only build", list)
		}
		list, err = qb.Retries(ctx, scope, Filters{Time: rng, Limit: 1, Offset: 1}, RetryWindow)
		if err != nil {
			t.Fatalf("Retries: %v", err)
		}
		if len(list) != 1 || list[0].CommandName == nil || *list[0].CommandName != "lint-chain" {
			t.Errorf("page 2 of 1: got %+v, want lint-chain", list)
		}
	})

	t.Run("window out of range", func(t *testing.T) {
		for _, w := range []time.Duration{0, -time.Minute, MaxRetryWindow + time.Second} {
			if _, err := qb.Retries(ctx, scope, Filters{Time: rng}, w); err == nil {
				t.Errorf("window %s: want an error", w)
			}
		}
	})
}
