package storage

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func errorReasonsTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live error reasons test")
	}
	ctx := context.Background()
	if err := MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func TestAdmitErrorReasons_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	w := NewWriter(pool)
	now := time.Now().UTC().Truncate(time.Second)
	const limit = 3

	vocabulary := func(t *testing.T, tenantID uuid.UUID) map[string]bool {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT reason FROM error_reasons WHERE tenant_id = $1`, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]bool{}
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			got[r] = true
		}
		return got
	}
	overflow := func(t *testing.T, tenantID uuid.UUID) int64 {
		t.Helper()
		var n int64
		err := pool.QueryRow(ctx, `SELECT coalesce((SELECT folded_count FROM error_reason_overflow WHERE tenant_id = $1), 0)`, tenantID).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("admits up to the cap, then folds and counts", func(t *testing.T) {
		tenant := uuid.New()
		admitted, folded, err := w.AdmitErrorReasons(ctx, tenant, map[string]int64{"a": 1, "b": 1, ErrorReasonOther: 4}, limit, now)
		if err != nil || folded != 0 || !admitted["a"] || !admitted["b"] || !admitted[ErrorReasonOther] {
			t.Fatalf("first batch: admitted=%v folded=%d err=%v", admitted, folded, err)
		}
		// One slot left: "d" is the more frequent newcomer, "c" and "e" fold.
		admitted, folded, err = w.AdmitErrorReasons(ctx, tenant, map[string]int64{"a": 2, "c": 1, "d": 3, "e": 1}, limit, now)
		if err != nil {
			t.Fatal(err)
		}
		if !admitted["a"] || !admitted["d"] || admitted["c"] || admitted["e"] || folded != 2 {
			t.Errorf("second batch: admitted=%v folded=%d, want a and d admitted, 2 folded", admitted, folded)
		}
		if got := vocabulary(t, tenant); len(got) != limit || got[ErrorReasonOther] {
			t.Errorf("vocabulary = %v, want a, b, d (other is never stored)", got)
		}
		if _, folded, _ = w.AdmitErrorReasons(ctx, tenant, map[string]int64{"c": 5}, limit, now); folded != 5 {
			t.Errorf("third batch folded %d, want 5", folded)
		}
		if n := overflow(t, tenant); n != 7 {
			t.Errorf("overflow = %d, want 7", n)
		}
		// Known reasons stay admitted when the cap is full.
		if admitted, folded, _ := w.AdmitErrorReasons(ctx, tenant, map[string]int64{"b": 1}, limit, now); !admitted["b"] || folded != 0 {
			t.Errorf("known reason: admitted=%v folded=%d", admitted, folded)
		}
	})

	t.Run("the cap is per tenant", func(t *testing.T) {
		full, empty := uuid.New(), uuid.New()
		if _, _, err := w.AdmitErrorReasons(ctx, full, map[string]int64{"x": 1, "y": 1, "z": 1}, limit, now); err != nil {
			t.Fatal(err)
		}
		admitted, folded, err := w.AdmitErrorReasons(ctx, empty, map[string]int64{"p": 1, "q": 1, "r": 1}, limit, now)
		if err != nil || folded != 0 || len(admitted) != 4 {
			t.Errorf("other tenant: admitted=%v folded=%d err=%v, want all three", admitted, folded, err)
		}
		if n := overflow(t, empty); n != 0 {
			t.Errorf("other tenant overflow = %d, want 0", n)
		}
	})

	t.Run("concurrent batches never exceed the cap", func(t *testing.T) {
		tenant := uuid.New()
		var wg sync.WaitGroup
		var mu sync.Mutex
		var admittedTotal, foldedTotal int64
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				admitted, folded, err := w.AdmitErrorReasons(ctx, tenant, map[string]int64{fmt.Sprintf("r%d", i): 1}, limit, now)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				admittedTotal += int64(len(admitted) - 1) // minus "other"
				foldedTotal += folded
			}()
		}
		wg.Wait()
		if n := len(vocabulary(t, tenant)); n != limit || admittedTotal != limit || foldedTotal != 10-limit {
			t.Errorf("vocabulary %d, admitted %d, folded %d; want %d, %d, %d", n, admittedTotal, foldedTotal, limit, limit, 10-limit)
		}
	})

	t.Run("sweep frees slots held by reasons no longer sent", func(t *testing.T) {
		tenant := uuid.New()
		old := now.AddDate(0, 0, -100)
		if _, _, err := w.AdmitErrorReasons(ctx, tenant, map[string]int64{"stale": 1, "fresh": 1, "kept": 1}, limit, old); err != nil {
			t.Fatal(err)
		}
		// Seen again today: last_seen moves forward (once a day), so these survive.
		if _, _, err := w.AdmitErrorReasons(ctx, tenant, map[string]int64{"fresh": 1, "kept": 1}, limit, now); err != nil {
			t.Fatal(err)
		}
		if _, err := SweepTenantInstalls(ctx, pool, tenant, 90); err != nil {
			t.Fatal(err)
		}
		if got := vocabulary(t, tenant); len(got) != 2 || got["stale"] {
			t.Fatalf("after sweep: %v, want fresh and kept", got)
		}
		admitted, folded, err := w.AdmitErrorReasons(ctx, tenant, map[string]int64{"new": 1}, limit, now)
		if err != nil || !admitted["new"] || folded != 0 {
			t.Errorf("freed slot: admitted=%v folded=%d err=%v", admitted, folded, err)
		}
	})
}

func TestErrorReasons_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant, other := uuid.New(), uuid.New()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	at := day.Add(10 * time.Hour)
	rng := TimeRange{From: day, To: day.AddDate(0, 0, 1)}

	var logs []LogRow
	event := func(tenantID uuid.UUID, command, errType, reason string, n int) {
		for range n {
			attrs := Attrs{"cli.error.type": errType}
			if reason != "" {
				attrs["cli.error.reason"] = reason
			}
			logs = append(logs, LogRow{
				Time: at, TenantID: tenantID, EventName: "cli.error.raised", CommandName: strp(command),
				ConsentTier: "basic", LogAttributes: attrs,
			})
		}
	}
	event(tenant, "deploy", "auth", "no-token", 3)
	event(tenant, "deploy", "auth", "expired-token", 1)
	event(tenant, "deploy", "network", "", 2) // an SDK that doesn't send reasons
	event(tenant, "init", "validation", "invalid-url", 1)
	event(other, "deploy", "auth", "leaked-reason", 5)
	if _, err := NewWriter(pool).InsertLogs(ctx, logs); err != nil {
		t.Fatalf("insert logs: %v", err)
	}

	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	query := func(t *testing.T, f Filters) map[string]int64 {
		t.Helper()
		f.Time = rng
		points, err := qb.ErrorReasons(ctx, scope, f, BucketDay)
		if err != nil {
			t.Fatalf("ErrorReasons: %v", err)
		}
		got := map[string]int64{}
		for _, p := range points {
			reason := "<none>"
			if p.ErrorReason != nil {
				reason = *p.ErrorReason
			}
			got[*p.CommandName+"/"+*p.ErrorType+"/"+reason] = p.ErrorCount
		}
		return got
	}

	t.Run("splits each category by reason, per command, tenant scoped", func(t *testing.T) {
		assertCounts(t, query(t, Filters{}), map[string]int64{
			"deploy/auth/no-token": 3, "deploy/auth/expired-token": 1,
			"deploy/network/<none>": 2, "init/validation/invalid-url": 1,
		})
	})
	t.Run("command filter", func(t *testing.T) {
		assertCounts(t, query(t, Filters{CommandNames: []string{"init"}}), map[string]int64{"init/validation/invalid-url": 1})
	})
	t.Run("default grouping is unchanged", func(t *testing.T) {
		points, err := qb.ErrorTypes(ctx, scope, Filters{Time: rng}, BucketDay)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, p := range points {
			got[*p.CommandName+"/"+*p.ErrorType] = p.ErrorCount
		}
		assertCounts(t, got, map[string]int64{"deploy/auth": 4, "deploy/network": 2, "init/validation": 1})
	})
}
