package storage

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSessionQueries_Live seeds a few installs whose sessions can be worked
// out by hand and checks the length distribution and command sequences
// against them.
func TestSessionQueries_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live session query test")
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
	// ev adds one event of install at offset from t0; an empty install is an
	// anonymous-tier event, which carries none.
	ev := func(tenantID uuid.UUID, install, event, command string, offset time.Duration) {
		attrs := Attrs{}
		tier := "anonymous"
		if install != "" {
			attrs["cli.install_id"] = install
			tier = "basic"
		}
		row := LogRow{Time: t0.Add(offset), TenantID: tenantID, EventName: event, ConsentTier: tier, LogAttributes: attrs}
		if command != "" {
			row.CommandName = strp(command)
		}
		logs = append(logs, row)
	}
	done := func(install, command string, offset time.Duration) {
		ev(tenant, install, "cli.command.completed", command, offset)
	}
	min := func(n int) time.Duration { return time.Duration(n) * time.Minute }

	// a: three commands, with a repeat and a gap of exactly 30 minutes (still
	// one session, 40 minutes long); 31 minutes later, a second session of one
	// command.
	done("a", "deploy", 0)
	done("a", "deploy", min(10))
	done("a", "status", min(40))
	done("a", "logs", min(71))
	// b interleaves with a in time: init then deploy, 3 minutes.
	done("b", "init", min(5))
	done("b", "deploy", min(8))
	// c: a single command.
	done("c", "deploy", min(20))
	// d: seven distinct commands a minute apart, past MaxSequenceLength.
	for i := 1; i <= 7; i++ {
		done("d", fmt.Sprintf("cmd%d", i), min(i-1))
	}
	// e: init then deploy exactly a minute apart, the <1m / 1-5m boundary.
	done("e", "init", min(100))
	done("e", "deploy", min(101))
	// Not sessions of anyone: anonymous-tier commands (no install id), other
	// event types, and another tenant's install.
	for i := 0; i < 5; i++ {
		ev(tenant, "", "cli.command.completed", "anon", min(i))
	}
	ev(tenant, "a", "cli.session.started", "", min(50))
	ev(other, "leak", "cli.command.completed", "leak", 0)
	ev(other, "leak", "cli.command.completed", "leak", min(2))

	if _, err := NewWriter(pool).InsertLogs(ctx, logs); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}

	bucketCounts := func(l SessionLengths) map[string]int64 {
		m := map[string]int64{}
		for _, b := range l.Buckets {
			m[b.Label] = b.Sessions
		}
		return m
	}
	assertBuckets := func(t *testing.T, got SessionLengths, want map[string]int64) {
		t.Helper()
		gotCounts := bucketCounts(got)
		for _, label := range sessionLengthLabels {
			if gotCounts[label] != want[label] {
				t.Errorf("bucket %q = %d, want %d", label, gotCounts[label], want[label])
			}
		}
	}

	t.Run("length distribution", func(t *testing.T) {
		got, err := qb.SessionLengthDistribution(ctx, scope, Filters{Time: rng})
		if err != nil {
			t.Fatalf("SessionLengthDistribution: %v", err)
		}
		// Sessions: a1 40m, a2 0, b 3m, c 0, d 6m, e 1m.
		if got.SessionCount != 6 {
			t.Errorf("session_count = %d, want 6", got.SessionCount)
		}
		if got.MedianSeconds == nil || *got.MedianSeconds != 120 { // between 1m and 3m
			t.Errorf("median = %v, want 120", got.MedianSeconds)
		}
		if got.GapSeconds != 1800 {
			t.Errorf("gap_seconds = %v, want 1800", got.GapSeconds)
		}
		assertBuckets(t, got, map[string]int64{"0": 2, "1-5m": 2, "5-15m": 1, "30m+": 1})
		if len(got.Buckets) != len(sessionLengthLabels) {
			t.Fatalf("got %d buckets, want %d", len(got.Buckets), len(sessionLengthLabels))
		}
		if b := got.Buckets[1]; b.MinSeconds != 0 || b.MaxSeconds == nil || *b.MaxSeconds != 60 {
			t.Errorf("<1m bounds = [%v, %v), want [0, 60)", b.MinSeconds, b.MaxSeconds)
		}
		if b := got.Buckets[len(got.Buckets)-1]; b.MinSeconds != 1800 || b.MaxSeconds != nil {
			t.Errorf("30m+ bounds = [%v, %v), want [1800, open)", b.MinSeconds, b.MaxSeconds)
		}
	})

	t.Run("command sequences", func(t *testing.T) {
		got, err := qb.SessionSequences(ctx, scope, Filters{Time: rng}, 1)
		if err != nil {
			t.Fatalf("SessionSequences: %v", err)
		}
		want := []SessionSequence{
			{[]string{"init", "deploy"}, 2},                       // b and e
			{[]string{"cmd1", "cmd2", "cmd3", "cmd4", "cmd5"}, 1}, // d, cut at MaxSequenceLength
			{[]string{"deploy"}, 1},                               // c
			{[]string{"deploy", "status"}, 1},                     // a1, repeat collapsed
			{[]string{"logs"}, 1},                                 // a2
		}
		assertSequences(t, got, want)
	})

	t.Run("min_length sets aside single-command sessions", func(t *testing.T) {
		got, err := qb.SessionSequences(ctx, scope, Filters{Time: rng}, 2)
		if err != nil {
			t.Fatalf("SessionSequences: %v", err)
		}
		want := []SessionSequence{
			{[]string{"init", "deploy"}, 2},
			{[]string{"cmd1", "cmd2", "cmd3", "cmd4", "cmd5"}, 1},
			{[]string{"deploy", "status"}, 1},
		}
		assertSequences(t, got, want)
	})

	t.Run("limit and offset page the ranking", func(t *testing.T) {
		got, err := qb.SessionSequences(ctx, scope, Filters{Time: rng, Limit: 2, Offset: 1}, 1)
		if err != nil {
			t.Fatalf("SessionSequences: %v", err)
		}
		assertSequences(t, got, []SessionSequence{
			{[]string{"cmd1", "cmd2", "cmd3", "cmd4", "cmd5"}, 1},
			{[]string{"deploy"}, 1},
		})
	})

	t.Run("filters narrow the events before sessions are built", func(t *testing.T) {
		f := Filters{Time: rng, CommandNames: []string{"deploy"}}
		got, err := qb.SessionLengthDistribution(ctx, scope, f)
		if err != nil {
			t.Fatalf("SessionLengthDistribution: %v", err)
		}
		// a: deploy@0 and @10m (10m), b, c, e: one deploy each.
		if got.SessionCount != 4 {
			t.Errorf("session_count = %d, want 4", got.SessionCount)
		}
		assertBuckets(t, got, map[string]int64{"0": 3, "5-15m": 1})
		seqs, err := qb.SessionSequences(ctx, scope, f, 1)
		if err != nil {
			t.Fatalf("SessionSequences: %v", err)
		}
		assertSequences(t, seqs, []SessionSequence{{[]string{"deploy"}, 4}})
	})

	t.Run("window outside the data and unknown tenant are empty", func(t *testing.T) {
		later := TimeRange{From: day.AddDate(0, 0, 1), To: day.AddDate(0, 0, 2)}
		got, err := qb.SessionLengthDistribution(ctx, scope, Filters{Time: later})
		if err != nil {
			t.Fatalf("SessionLengthDistribution: %v", err)
		}
		if got.SessionCount != 0 || got.MedianSeconds != nil {
			t.Errorf("empty window: count = %d, median = %v; want 0, nil", got.SessionCount, got.MedianSeconds)
		}
		assertBuckets(t, got, nil)
		seqs, err := qb.SessionSequences(ctx, scope, Filters{Time: later}, 1)
		if err != nil || len(seqs) != 0 {
			t.Errorf("empty window sequences = %v, %v; want none", seqs, err)
		}
	})

	t.Run("another tenant sees only its own sessions", func(t *testing.T) {
		otherScope, err := NewTenantScope(other)
		if err != nil {
			t.Fatalf("NewTenantScope: %v", err)
		}
		got, err := qb.SessionLengthDistribution(ctx, otherScope, Filters{Time: rng})
		if err != nil {
			t.Fatalf("SessionLengthDistribution: %v", err)
		}
		if got.SessionCount != 1 { // leak: two commands 2m apart
			t.Errorf("session_count = %d, want 1", got.SessionCount)
		}
		assertBuckets(t, got, map[string]int64{"1-5m": 1})
		seqs, err := qb.SessionSequences(ctx, otherScope, Filters{Time: rng}, 1)
		if err != nil {
			t.Fatalf("SessionSequences: %v", err)
		}
		assertSequences(t, seqs, []SessionSequence{{[]string{"leak"}, 1}})
	})
}

func assertSequences(t *testing.T, got, want []SessionSequence) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d sequences %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if !slices.Equal(got[i].Commands, want[i].Commands) || got[i].Sessions != want[i].Sessions {
			t.Errorf("sequence %d = %v x%d, want %v x%d", i, got[i].Commands, got[i].Sessions, want[i].Commands, want[i].Sessions)
		}
	}
}
