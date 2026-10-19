package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAdmitCounters_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	w := NewWriter(pool)
	now := time.Now().UTC().Truncate(time.Second)
	const nameLimit, labelLimit = 3, 2

	key := func(name, label string) CounterKey { return CounterKey{name, label} }
	vocabulary := func(t *testing.T, tenantID uuid.UUID) (names map[string]bool, labels map[CounterKey]bool) {
		t.Helper()
		names, labels = map[string]bool{}, map[CounterKey]bool{}
		rows, err := pool.Query(ctx, `SELECT counter_name FROM counter_names WHERE tenant_id = $1`, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			names[n] = true
		}
		rows.Close()
		rows, err = pool.Query(ctx, `SELECT counter_name, label FROM counter_labels WHERE tenant_id = $1`, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k CounterKey
			if err := rows.Scan(&k.Name, &k.Label); err != nil {
				t.Fatal(err)
			}
			labels[k] = true
		}
		return names, labels
	}
	overflow := func(t *testing.T, tenantID uuid.UUID) (names, labels int64) {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT coalesce(sum(folded_names), 0), coalesce(sum(folded_labels), 0) FROM counter_overflow WHERE tenant_id = $1`, tenantID).Scan(&names, &labels)
		if err != nil {
			t.Fatal(err)
		}
		return names, labels
	}

	t.Run("name cap folds whole names, label cap folds labels per name", func(t *testing.T) {
		tenant := uuid.New()
		adm, err := w.AdmitCounters(ctx, tenant, map[CounterKey]int64{
			key("scans", ""): 1, key("verbs", "get"): 4, key("verbs", "post"): 2, key("verbs", "put"): 1,
			key(CounterOther, "x"): 9, key("verbs", CounterOther): 3,
		}, nameLimit, labelLimit, now)
		if err != nil {
			t.Fatal(err)
		}
		// Two labels fit under verbs: get and post, the most frequent; put folds.
		if !adm.Names["scans"] || !adm.Names["verbs"] || !adm.Labels[key("verbs", "get")] || !adm.Labels[key("verbs", "post")] || adm.Labels[key("verbs", "put")] {
			t.Errorf("first batch: %+v", adm)
		}
		if adm.FoldedNames != 0 || adm.FoldedLabels != 1 {
			t.Errorf("first batch folded names=%d labels=%d, want 0 and 1", adm.FoldedNames, adm.FoldedLabels)
		}

		// One name slot left: "tools" is the more frequent newcomer (over two
		// labels), "aaa" and "zzz" fold with all their data points.
		adm, err = w.AdmitCounters(ctx, tenant, map[CounterKey]int64{
			key("tools", "nmap"): 2, key("tools", "hydra"): 2, key("aaa", ""): 3, key("zzz", "q"): 1, key("verbs", "get"): 1,
		}, nameLimit, labelLimit, now)
		if err != nil {
			t.Fatal(err)
		}
		if !adm.Names["tools"] || adm.Names["aaa"] || adm.Names["zzz"] || !adm.Labels[key("tools", "hydra")] || !adm.Labels[key("tools", "nmap")] {
			t.Errorf("second batch: %+v", adm)
		}
		if adm.FoldedNames != 4 || adm.FoldedLabels != 0 {
			t.Errorf("second batch folded names=%d labels=%d, want 4 and 0", adm.FoldedNames, adm.FoldedLabels)
		}

		names, labels := vocabulary(t, tenant)
		if len(names) != nameLimit || names[CounterOther] || len(labels) != 4 || labels[key("verbs", CounterOther)] {
			t.Errorf("vocabulary = %v / %v, want 3 names and 4 labels, other never stored", names, labels)
		}
		if n, l := overflow(t, tenant); n != 4 || l != 1 {
			t.Errorf("overflow = names %d labels %d, want 4 and 1", n, l)
		}

		// Known values stay admitted when the caps are full.
		adm, err = w.AdmitCounters(ctx, tenant, map[CounterKey]int64{key("verbs", "post"): 1, key("scans", ""): 1}, nameLimit, labelLimit, now)
		if err != nil || !adm.Names["verbs"] || !adm.Names["scans"] || !adm.Labels[key("verbs", "post")] || adm.FoldedNames+adm.FoldedLabels != 0 {
			t.Errorf("known values: %+v err=%v", adm, err)
		}
	})

	t.Run("the caps are per tenant and per name", func(t *testing.T) {
		full, empty := uuid.New(), uuid.New()
		if _, err := w.AdmitCounters(ctx, full, map[CounterKey]int64{key("a", "1"): 1, key("a", "2"): 1, key("b", ""): 1, key("c", ""): 1}, nameLimit, labelLimit, now); err != nil {
			t.Fatal(err)
		}
		adm, err := w.AdmitCounters(ctx, empty, map[CounterKey]int64{key("a", "1"): 1, key("a", "3"): 1, key("x", ""): 1, key("y", ""): 1}, nameLimit, labelLimit, now)
		if err != nil || adm.FoldedNames+adm.FoldedLabels != 0 || len(adm.Names) != 4 || len(adm.Labels) != 2 {
			t.Errorf("other tenant: %+v err=%v, want everything admitted", adm, err)
		}
		// Name "a" has no label room left in full, but "b" still has its own.
		adm, err = w.AdmitCounters(ctx, full, map[CounterKey]int64{key("a", "3"): 1, key("b", "1"): 1, key("b", "2"): 1}, nameLimit, labelLimit, now)
		if err != nil || adm.Labels[key("a", "3")] || !adm.Labels[key("b", "1")] || !adm.Labels[key("b", "2")] || adm.FoldedLabels != 1 {
			t.Errorf("label caps per name: %+v err=%v", adm, err)
		}
	})

	t.Run("concurrent batches never exceed the name cap", func(t *testing.T) {
		tenant := uuid.New()
		var wg sync.WaitGroup
		var mu sync.Mutex
		var admittedTotal, foldedTotal int64
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				adm, err := w.AdmitCounters(ctx, tenant, map[CounterKey]int64{key(fmt.Sprintf("c%d", i), "l"): 1}, nameLimit, labelLimit, now)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				admittedTotal += int64(len(adm.Names) - 1) // minus "other"
				foldedTotal += adm.FoldedNames
			}()
		}
		wg.Wait()
		names, labels := vocabulary(t, tenant)
		if len(names) != nameLimit || len(labels) != nameLimit || admittedTotal != nameLimit || foldedTotal != 10-nameLimit {
			t.Errorf("names %d, labels %d, admitted %d, folded %d; want %d, %d, %d, %d",
				len(names), len(labels), admittedTotal, foldedTotal, nameLimit, nameLimit, nameLimit, 10-nameLimit)
		}
	})

	t.Run("sweep frees slots held by values no longer sent", func(t *testing.T) {
		tenant := uuid.New()
		old := now.AddDate(0, 0, -100)
		if _, err := w.AdmitCounters(ctx, tenant, map[CounterKey]int64{key("stale", "x"): 1, key("kept", "old"): 1, key("kept", "new"): 1}, nameLimit, labelLimit, old); err != nil {
			t.Fatal(err)
		}
		// Seen again today: last_seen moves forward (once a day).
		if _, err := w.AdmitCounters(ctx, tenant, map[CounterKey]int64{key("kept", "new"): 1}, nameLimit, labelLimit, now); err != nil {
			t.Fatal(err)
		}
		if _, err := SweepTenantInstalls(ctx, pool, tenant, 90); err != nil {
			t.Fatal(err)
		}
		names, labels := vocabulary(t, tenant)
		if len(names) != 1 || !names["kept"] || len(labels) != 1 || !labels[key("kept", "new")] {
			t.Fatalf("after sweep: %v / %v, want kept with label new", names, labels)
		}
		adm, err := w.AdmitCounters(ctx, tenant, map[CounterKey]int64{key("kept", "again"): 1}, nameLimit, labelLimit, now)
		if err != nil || !adm.Labels[key("kept", "again")] || adm.FoldedLabels != 0 {
			t.Errorf("freed label slot: %+v err=%v", adm, err)
		}
	})
}

// TestCounters_Live covers the counter rollups: totals per name over the
// window, the per-name series at hour and day, labels, filters, the overflow
// report, and tenant isolation.
func TestCounters_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant, other := uuid.New(), uuid.New()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)

	var rows []MetricRow
	point := func(tenantID uuid.UUID, at time.Time, name, label, command string, ci bool, v float64) {
		attrs := Attrs{"cli.counter.name": name}
		if label != "" {
			attrs["cli.counter.label"] = label
		}
		rows = append(rows, MetricRow{
			Time: at, TenantID: tenantID, MetricName: "cli.counter", MetricType: "sum", Unit: strp("1"), Value: f64p(v),
			CommandName: strp(command), CLIVersion: strp("1.0.0"), OS: strp("linux"), Arch: strp("amd64"), IsCI: boolp(ci),
			ConsentTier: "anonymous", DatapointAttributes: attrs,
		})
	}
	h10, h11 := day.Add(10*time.Hour), day.Add(11*time.Hour)
	point(tenant, h10, "rules.generated", "get", "generate", false, 5)
	point(tenant, h10, "rules.generated", "get", "generate", true, 2)
	point(tenant, h11, "rules.generated", "post", "generate", false, 3)
	point(tenant, h11, "rules.generated", "", "generate", false, 1)
	point(tenant, h10, "schemes.seen", "bearer", "scan", false, 4)
	point(other, h10, "leaked", "x", "scan", false, 100)
	point(other, h10, "rules.generated", "get", "generate", false, 100)
	// Not a counter: must not reach the rollup.
	rows = append(rows, MetricRow{Time: h10, TenantID: tenant, MetricName: "cli.errors", MetricType: "sum", Value: f64p(50),
		ConsentTier: "basic", DatapointAttributes: Attrs{"cli.counter.name": "rules.generated"}})

	if _, err := NewWriter(pool).InsertMetrics(ctx, rows); err != nil {
		t.Fatalf("insert metrics: %v", err)
	}
	for _, cagg := range []string{"cagg_counters_hourly", "cagg_counters_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO counter_overflow (tenant_id, folded_names, folded_labels, last_folded_at) VALUES ($1, 7, 2, $2)`, tenant, h11); err != nil {
		t.Fatal(err)
	}

	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	rng := TimeRange{From: day, To: day.AddDate(0, 0, 1)}
	labelTotals := func(d CounterDetail) map[string]float64 {
		m := map[string]float64{}
		for _, l := range d.Labels {
			k := "<none>"
			if l.Label != nil {
				k = *l.Label
			}
			m[k] = l.Total
		}
		return m
	}

	t.Run("list totals per name, largest first, with the overflow", func(t *testing.T) {
		list, err := qb.Counters(ctx, scope, Filters{Time: rng})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Counters) != 2 {
			t.Fatalf("got %+v, want rules.generated and schemes.seen only", list.Counters)
		}
		r, s := list.Counters[0], list.Counters[1]
		if r.Name != "rules.generated" || r.Total != 11 || r.DatapointCount != 4 || r.LabelCount != 2 {
			t.Errorf("first = %+v, want rules.generated total 11 over 4 points, 2 labels", r)
		}
		if s.Name != "schemes.seen" || s.Total != 4 || s.LabelCount != 1 {
			t.Errorf("second = %+v, want schemes.seen total 4, 1 label", s)
		}
		if list.Overflow.FoldedNames != 7 || list.Overflow.FoldedLabels != 2 || list.Overflow.LastFoldedAt == nil {
			t.Errorf("overflow = %+v, want 7 names and 2 labels folded", list.Overflow)
		}
	})

	t.Run("list filters, and an empty overflow for a tenant without folds", func(t *testing.T) {
		ci := true
		list, err := qb.Counters(ctx, scope, Filters{Time: rng, IsCI: &ci})
		if err != nil || len(list.Counters) != 1 || list.Counters[0].Total != 2 {
			t.Errorf("is_ci=true: %+v err=%v, want rules.generated total 2", list.Counters, err)
		}
		list, err = qb.Counters(ctx, scope, Filters{Time: rng, CommandNames: []string{"scan"}})
		if err != nil || len(list.Counters) != 1 || list.Counters[0].Name != "schemes.seen" {
			t.Errorf("command=scan: %+v err=%v", list.Counters, err)
		}
		otherScope, _ := NewTenantScope(uuid.New())
		list, err = qb.Counters(ctx, otherScope, Filters{Time: rng})
		if err != nil || len(list.Counters) != 0 || list.Overflow.FoldedNames != 0 || list.Overflow.LastFoldedAt != nil {
			t.Errorf("empty tenant: %+v err=%v", list, err)
		}
	})

	t.Run("series per hour, labels over the window", func(t *testing.T) {
		d, err := qb.CounterSeries(ctx, scope, Filters{Time: rng}, "rules.generated", nil, BucketHour)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Series) != 2 || !d.Series[0].Bucket.Equal(h10) || d.Series[0].Total != 7 || d.Series[1].Total != 4 {
			t.Errorf("series = %+v, want 7 at 10:00 and 4 at 11:00", d.Series)
		}
		if got := labelTotals(d); len(got) != 3 || got["get"] != 7 || got["post"] != 3 || got["<none>"] != 1 {
			t.Errorf("labels = %v, want get 7, post 3, none 1", got)
		}
		if d.Labels[0].Label == nil || *d.Labels[0].Label != "get" {
			t.Errorf("labels should be largest first: %+v", d.Labels)
		}
	})

	t.Run("series per day, narrowed to a label", func(t *testing.T) {
		label := "get"
		d, err := qb.CounterSeries(ctx, scope, Filters{Time: rng}, "rules.generated", &label, BucketDay)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Series) != 1 || !d.Series[0].Bucket.Equal(day) || d.Series[0].Total != 7 || d.Series[0].DatapointCount != 2 {
			t.Errorf("series = %+v, want one day of 7 over 2 points", d.Series)
		}
		// The label breakdown ignores the label filter.
		if got := labelTotals(d); len(got) != 3 {
			t.Errorf("labels = %v, want all three", got)
		}
	})

	t.Run("another tenant's counters are never returned", func(t *testing.T) {
		d, err := qb.CounterSeries(ctx, scope, Filters{Time: rng}, "leaked", nil, BucketDay)
		if err != nil || len(d.Series) != 0 || len(d.Labels) != 0 {
			t.Errorf("leaked: %+v err=%v", d, err)
		}
	})

	t.Run("unsupported bucket is rejected", func(t *testing.T) {
		if _, err := qb.CounterSeries(ctx, scope, Filters{Time: rng}, "rules.generated", nil, Bucket("minute")); err == nil {
			t.Fatal("expected error for unsupported bucket")
		}
	})
}
