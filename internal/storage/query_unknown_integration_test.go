package storage

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestUnknownCommands_Live seeds attempts and invocations that can be counted
// by hand, and checks the rate, the top-K and, above all, that a suggestion
// that is not a command the tenant has reported never comes back.
func TestUnknownCommands_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live unknown commands query test")
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

	day1 := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -4)
	day2 := day1.AddDate(0, 0, 1)
	rng := TimeRange{From: day1, To: day2.AddDate(0, 0, 1)}

	// Invocations: 10 on day 1, 40 on day 2 (30 of them on darwin), as spans
	// that the command stats rollups are built from. deploy and push are the
	// commands the tenant has reported.
	var spans []TraceRow
	span := func(tenantID uuid.UUID, at time.Time, cmd, goos string) {
		spans = append(spans, TraceRow{
			Time: at, TenantID: tenantID, TraceID: uuid.NewString(), SpanID: uuid.NewString(),
			SpanName: "cli.command." + cmd, StartTime: at, EndTime: at.Add(time.Second), DurationMs: 1000,
			StatusCode: 1, CommandName: strp(cmd), ExitCode: i32p(0), ConsentTier: "anonymous",
			CLIVersion: strp("1.0.0"), OS: strp(goos), Arch: strp("amd64"),
		})
	}
	for i := range 10 {
		span(tenant, day1.Add(time.Duration(i)*time.Minute), "deploy", "linux")
	}
	for i := range 40 {
		goos := "linux"
		if i < 30 {
			goos = "darwin"
		}
		span(tenant, day2.Add(time.Duration(i)*time.Minute), "push", goos)
	}
	// Another tenant's traffic, and a command only it has reported.
	for i := range 100 {
		span(other, day1.Add(time.Duration(i)*time.Minute), "secret-cmd", "linux")
	}
	if _, err := NewWriter(pool).InsertTraces(ctx, spans); err != nil {
		t.Fatalf("InsertTraces: %v", err)
	}
	for _, cagg := range []string{"cagg_command_stats_hourly", "cagg_command_stats_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	var logs []LogRow
	n := 0
	// attempt adds a cli.command.unknown. A nil suggestion is an attempt that
	// carries none, as at anonymous tier.
	attempt := func(tenantID uuid.UUID, at time.Time, goos string, suggestion *string) {
		n++
		tier, attrs := "anonymous", Attrs{}
		if suggestion != nil {
			tier, attrs = "basic", Attrs{"cli.command.suggestion": *suggestion}
		}
		logs = append(logs, LogRow{
			Time: at.Add(time.Duration(n) * time.Second), TenantID: tenantID, EventName: "cli.command.unknown",
			ConsentTier: tier, CLIVersion: strp("1.0.0"), OS: strp(goos), Arch: strp("amd64"), LogAttributes: attrs,
		})
	}
	// Day 1: 2 attempts. Day 2: 8.
	attempt(tenant, day1, "linux", strp("deploy"))
	attempt(tenant, day1, "linux", nil)
	for range 3 {
		attempt(tenant, day2, "darwin", strp("push"))
	}
	for range 2 {
		attempt(tenant, day2, "linux", strp("deploy"))
	}
	attempt(tenant, day2, "linux", nil)
	// Typed text a misbehaving client put in the suggestion, and a command only
	// another tenant has reported: both must fold into other.
	attempt(tenant, day2, "linux", strp("hunter2-my-password"))
	attempt(tenant, day2, "linux", strp("hunter2-my-password"))
	attempt(tenant, day2, "linux", strp("secret-cmd"))
	attempt(tenant, day2, "linux", strp(""))
	// The other tenant's attempts, a command event and an unrelated event with
	// the same attribute must never count.
	attempt(other, day2, "linux", strp("secret-cmd"))
	logs = append(logs,
		LogRow{
			Time: day2.Add(time.Hour), TenantID: tenant, EventName: "cli.command.completed", ConsentTier: "basic",
			CLIVersion: strp("1.0.0"), OS: strp("linux"), Arch: strp("amd64"),
			LogAttributes: Attrs{"cli.command.suggestion": "deploy"},
		},
		LogRow{
			Time: day2.AddDate(0, 0, 3), TenantID: tenant, EventName: "cli.command.unknown", ConsentTier: "basic",
			CLIVersion: strp("1.0.0"), OS: strp("linux"), Arch: strp("amd64"),
			LogAttributes: Attrs{"cli.command.suggestion": "deploy"},
		},
	)
	if _, err := NewWriter(pool).InsertLogs(ctx, logs); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, err := NewTenantScope(tenant)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	get := func(t *testing.T, f Filters, b Bucket) UnknownCommands {
		t.Helper()
		f.Time = rng
		res, err := qb.UnknownCommands(ctx, scope, f, b)
		if err != nil {
			t.Fatalf("UnknownCommands: %v", err)
		}
		return res
	}
	values := func(s UnknownCommandSuggestions) map[string]int64 {
		m := map[string]int64{}
		for _, v := range s.Values {
			m[v.Suggestion] = v.AttemptCount
		}
		return m
	}
	eq := func(got, want map[string]int64) bool {
		if len(got) != len(want) {
			return false
		}
		for k, v := range want {
			if got[k] != v {
				return false
			}
		}
		return true
	}

	t.Run("rate per day, with attempts and invocations of the tenant only", func(t *testing.T) {
		res := get(t, Filters{}, BucketDay)
		if len(res.Series) != 2 {
			t.Fatalf("series = %+v, want 2 days", res.Series)
		}
		d1, d2 := res.Series[0], res.Series[1]
		if !d1.Bucket.Equal(day1) || d1.AttemptCount != 2 || d1.InvocationCount != 10 || math.Abs(d1.AttemptRate-0.2) > 1e-9 {
			t.Errorf("day 1 = %+v, want 2 attempts / 10 invocations = 0.2", d1)
		}
		if !d2.Bucket.Equal(day2) || d2.AttemptCount != 10 || d2.InvocationCount != 40 || math.Abs(d2.AttemptRate-0.25) > 1e-9 {
			t.Errorf("day 2 = %+v, want 10 attempts / 40 invocations = 0.25", d2)
		}
	})

	t.Run("suggestions that are not reported commands fold into other", func(t *testing.T) {
		s := get(t, Filters{}, BucketDay).Suggestions
		if !eq(values(s), map[string]int64{"push": 3, "deploy": 3}) {
			t.Errorf("values = %v, want push 3, deploy 3", values(s))
		}
		// Ties by name: deploy before push.
		if len(s.Values) != 2 || s.Values[0].Suggestion != "deploy" || s.Values[1].Suggestion != "push" {
			t.Errorf("order = %+v, want deploy then push", s.Values)
		}
		// 12 attempts: 3 without a suggestion (two omitted it, one sent an
		// empty string, which counts as none) and 9 with one.
		if s.AttemptCount != 12 || s.SuggestedAttempts != 9 {
			t.Errorf("attempt_count, suggested_attempts = %d, %d; want 12, 9", s.AttemptCount, s.SuggestedAttempts)
		}
		// hunter2 x2 and the other tenant's command: 3 attempts, 2 distinct.
		if s.Other.AttemptCount != 3 || s.Other.SuggestionCount != 2 {
			t.Errorf("other = %+v, want 3 attempts, 2 suggestions", s.Other)
		}
		var sum int64
		for _, v := range s.Values {
			sum += v.AttemptCount
		}
		if sum+s.Other.AttemptCount != s.SuggestedAttempts {
			t.Errorf("values %d + other %d != suggested %d", sum, s.Other.AttemptCount, s.SuggestedAttempts)
		}
	})

	t.Run("no typed text in the response", func(t *testing.T) {
		res := get(t, Filters{}, BucketDay)
		for _, v := range res.Suggestions.Values {
			if strings.Contains(v.Suggestion, "hunter2") || v.Suggestion == "secret-cmd" || v.Suggestion == "" {
				t.Errorf("unreported suggestion %q was returned", v.Suggestion)
			}
		}
	})

	t.Run("top-K folds the rest into other", func(t *testing.T) {
		s := get(t, Filters{Limit: 1}, BucketDay).Suggestions
		if len(s.Values) != 1 || s.Values[0].Suggestion != "deploy" {
			t.Fatalf("values = %+v, want only deploy", s.Values)
		}
		// push (3) past the cap, plus the 3 unreported attempts.
		if s.Other.AttemptCount != 6 || s.Other.SuggestionCount != 3 {
			t.Errorf("other = %+v, want 6 attempts, 3 suggestions", s.Other)
		}
		if s.Values[0].AttemptCount+s.Other.AttemptCount != s.SuggestedAttempts {
			t.Errorf("values and other don't add up to suggested_attempts %d", s.SuggestedAttempts)
		}
	})

	t.Run("filters apply to both sides, command does not", func(t *testing.T) {
		res := get(t, Filters{OSes: []string{"darwin"}}, BucketDay)
		if len(res.Series) != 1 {
			t.Fatalf("series = %+v, want only day 2", res.Series)
		}
		if p := res.Series[0]; p.AttemptCount != 3 || p.InvocationCount != 30 || math.Abs(p.AttemptRate-0.1) > 1e-9 {
			t.Errorf("darwin = %+v, want 3 / 30 = 0.1", p)
		}
		if got := values(res.Suggestions); !eq(got, map[string]int64{"push": 3}) {
			t.Errorf("darwin suggestions = %v, want push 3", got)
		}

		// A command filter would narrow the invocations only, so it is ignored.
		res = get(t, Filters{CommandNames: []string{"deploy"}}, BucketDay)
		if p := res.Series[1]; p.AttemptCount != 10 || p.InvocationCount != 40 {
			t.Errorf("with command filter, day 2 = %+v, want 10 / 40 (unchanged)", p)
		}
	})

	t.Run("week bucket sums the days", func(t *testing.T) {
		res := get(t, Filters{}, BucketWeek)
		var attempts, invocations int64
		for _, p := range res.Series {
			attempts += p.AttemptCount
			invocations += p.InvocationCount
		}
		if attempts != 12 || invocations != 50 {
			t.Errorf("weeks sum to %d attempts / %d invocations, want 12 / 50", attempts, invocations)
		}
	})

	t.Run("a bucket with attempts but no invocations has rate 0", func(t *testing.T) {
		later := Filters{Time: TimeRange{From: day2.AddDate(0, 0, 3), To: day2.AddDate(0, 0, 4)}}
		res, err := qb.UnknownCommands(ctx, scope, later, BucketDay)
		if err != nil {
			t.Fatalf("UnknownCommands: %v", err)
		}
		if len(res.Series) != 1 || res.Series[0].AttemptCount != 1 || res.Series[0].InvocationCount != 0 || res.Series[0].AttemptRate != 0 {
			t.Errorf("series = %+v, want one bucket with 1 attempt and no rate", res.Series)
		}
	})

	t.Run("empty window", func(t *testing.T) {
		empty := Filters{Time: TimeRange{From: day1.AddDate(0, 0, -30), To: day1.AddDate(0, 0, -29)}}
		res, err := qb.UnknownCommands(ctx, scope, empty, BucketDay)
		if err != nil {
			t.Fatalf("UnknownCommands: %v", err)
		}
		if res.Series == nil || len(res.Series) != 0 || res.Suggestions.Values == nil || res.Suggestions.AttemptCount != 0 {
			t.Errorf("empty window = %+v, want empty non-nil lists and zero counts", res)
		}
	})
}
