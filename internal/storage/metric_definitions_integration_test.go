package storage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestApplyMetricDefinitions_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	w := NewWriter(pool)
	qb := NewQueryBuilder(pool)
	tenant := uuid.New()
	scope, _ := NewTenantScope(tenant)
	now := time.Now().UTC().Truncate(time.Second)
	def := func(name, kind, title string) MetricDefinitionInput {
		spec, _ := json.Marshal(map[string]string{"name": name, "kind": kind, "title": title})
		return MetricDefinitionInput{Name: name, Kind: kind, Spec: spec}
	}
	apply := func(t *testing.T, service string, at time.Time, defs ...MetricDefinitionInput) ApplyResult {
		t.Helper()
		res, err := w.ApplyMetricDefinitions(ctx, tenant, service, defs, "test", at)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	count := func(r ApplyResult) [4]int {
		return [4]int{len(r.Created), len(r.Updated), len(r.Unchanged), len(r.Deprecated)}
	}

	res := apply(t, "vulnapi", now, def("vulns.found", "counter", "Vulns"), def("scan.run", "duration", "Scan time"))
	if count(res) != [4]int{2, 0, 0, 0} {
		t.Errorf("first apply = %+v, want 2 created", res)
	}
	res = apply(t, "vulnapi", now.Add(time.Hour), def("vulns.found", "counter", "Vulns"), def("scan.run", "duration", "Scan time"))
	if count(res) != [4]int{0, 0, 2, 0} {
		t.Errorf("same file again = %+v, want 2 unchanged", res)
	}
	res = apply(t, "vulnapi", now.Add(2*time.Hour), def("vulns.found", "counter", "Vulnerabilities found"))
	if count(res) != [4]int{0, 1, 0, 1} || res.Deprecated[0] != "scan.run" {
		t.Errorf("title changed, scan.run removed = %+v, want 1 updated, scan.run deprecated", res)
	}
	// Another CLI of the tenant is a separate catalog.
	if res := apply(t, "jwtop", now, def("tokens.cracked", "counter", "Tokens")); count(res) != [4]int{1, 0, 0, 0} {
		t.Errorf("jwtop apply = %+v", res)
	}

	defs, err := qb.MetricDefinitions(ctx, scope, []string{"vulnapi"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 {
		t.Fatalf("vulnapi definitions = %+v, want vulns.found and the deprecated scan.run", defs)
	}
	scan, vulns := defs[0], defs[1]
	if scan.Name != "scan.run" || scan.DeprecatedAt == nil || !scan.Declared {
		t.Errorf("scan.run = %+v, want declared and deprecated", scan)
	}
	var spec map[string]string
	if err := json.Unmarshal(vulns.Definition, &spec); err != nil || spec["title"] != "Vulnerabilities found" || vulns.DeprecatedAt != nil ||
		vulns.UpdatedBy == nil || *vulns.UpdatedBy != "test" || !vulns.UpdatedAt.Equal(now.Add(2*time.Hour)) {
		t.Errorf("vulns.found = %+v (%s), want the new title", vulns, vulns.Definition)
	}

	// Re-adding a deprecated metric restores it.
	res = apply(t, "vulnapi", now.Add(3*time.Hour), def("vulns.found", "counter", "Vulnerabilities found"), def("scan.run", "duration", "Scan time"))
	if count(res) != [4]int{0, 1, 1, 0} || res.Updated[0] != "scan.run" {
		t.Errorf("re-add = %+v, want scan.run updated", res)
	}
}

func TestMetricDefinitionsDiscovery_Live(t *testing.T) {
	ctx, pool := errorReasonsTestPool(t)
	tenant := uuid.New()
	scope, _ := NewTenantScope(tenant)
	now := time.Now().UTC()
	at := now.Add(-48 * time.Hour).Truncate(time.Hour)

	var rows []MetricRow
	counter := func(service, name string) {
		rows = append(rows, MetricRow{
			Time: at, TenantID: tenant, MetricName: "cli.counter", MetricType: "sum", Unit: strp("1"), Value: f64p(1),
			ConsentTier: "anonymous", ResourceAttributes: Attrs{"service.name": service}, DatapointAttributes: Attrs{"cli.counter.name": name},
		})
	}
	counter("vulnapi", "vulns.found")  // declared below
	counter("vulnapi", "rules.loaded") // undeclared
	counter("vulnapi", "other")        // folded: never listed
	counter("jwtop", "tokens.cracked") // another CLI
	n := int64(1)
	rows = append(rows, MetricRow{
		Time: at, TenantID: tenant, MetricName: "cli.measure", MetricType: "histogram", Unit: strp("ms"),
		HistogramCount: &n, HistogramBucketBounds: MeasureBounds, HistogramBucketCounts: make([]int64, MeasureBucketCount),
		ConsentTier: "anonymous", ResourceAttributes: Attrs{"service.name": "vulnapi"}, DatapointAttributes: Attrs{"cli.measure.name": "vulns.found"},
	})
	if _, err := NewWriter(pool).InsertMetrics(ctx, rows); err != nil {
		t.Fatal(err)
	}
	for _, cagg := range []string{"cagg_counters_hourly", "cagg_counters_daily", "cagg_measures_hourly", "cagg_measures_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}
	spec := json.RawMessage(`{"name":"vulns.found","kind":"counter","title":"Vulns"}`)
	if _, err := NewWriter(pool).ApplyMetricDefinitions(ctx, tenant, "vulnapi", []MetricDefinitionInput{{Name: "vulns.found", Kind: "counter", Spec: spec}}, "test", now); err != nil {
		t.Fatal(err)
	}

	defs, err := NewQueryBuilder(pool).MetricDefinitions(ctx, scope, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		service, name, kind string
		declared            bool
	}
	var got []entry
	for _, d := range defs {
		got = append(got, entry{d.Service, d.Name, d.Kind, d.Declared})
	}
	want := []entry{
		{"jwtop", "tokens.cracked", "counter", false},
		{"vulnapi", "rules.loaded", "counter", false},
		{"vulnapi", "vulns.found", "counter", true},
		{"vulnapi", "vulns.found", "duration", false}, // same name, other kind: listed apart
	}
	if len(got) != len(want) {
		t.Fatalf("definitions = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	defs, err = NewQueryBuilder(pool).MetricDefinitions(ctx, scope, []string{"jwtop"}, now)
	if err != nil || len(defs) != 1 {
		t.Errorf("jwtop only = %+v err=%v", defs, err)
	}
	// Outside the discovery window, an undeclared metric is no longer listed.
	defs, err = NewQueryBuilder(pool).MetricDefinitions(ctx, scope, []string{"jwtop"}, now.Add(MetricDefinitionDiscoveryWindow+72*time.Hour))
	if err != nil || len(defs) != 0 {
		t.Errorf("past the window = %+v err=%v", defs, err)
	}
}
