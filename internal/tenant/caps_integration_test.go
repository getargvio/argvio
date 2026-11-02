package tenant

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/storage"
)

// TestMetricCaps_Live round-trips the per-tenant vendor-metric caps through
// tenant_config: set ones come back, unset ones stay nil (the global value),
// and the database refuses a non-positive cap.
func TestMetricCaps_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live tenant caps test")
	}
	ctx := context.Background()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)
	ten, err := store.CreateTenant(ctx, "caps-"+time.Now().Format("150405.000000000"), "Caps")
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := store.LookupByTenantID(ctx, ten.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Config.MetricCaps != (MetricCaps{}) {
		t.Errorf("a new tenant has no overrides, got %+v", resolved.Config.MetricCaps)
	}

	names, dims, values := 500, 4, 300
	if err := store.UpsertConfig(ctx, Config{
		TenantID: ten.ID, TierCeiling: "basic", TierEnforcementMode: "strip",
		MetricCaps:    MetricCaps{MaxCounterNames: &names, MaxMetricDims: &dims, MaxDimValuesPerKey: &values},
		StrictMetrics: true,
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err = store.LookupByTenantID(ctx, ten.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := resolved.Config.MetricCaps
	if !resolved.Config.StrictMetrics {
		t.Error("strict_metrics should round-trip")
	}
	if c.MaxCounterNames == nil || *c.MaxCounterNames != 500 || c.MaxMetricDims == nil || *c.MaxMetricDims != 4 ||
		c.MaxDimValuesPerKey == nil || *c.MaxDimValuesPerKey != 300 {
		t.Errorf("set caps = %+v, want counter names 500, dims 4, dim values 300", c)
	}
	if c.MaxCounterLabelsPerName != nil || c.MaxMeasureNames != nil || c.MaxMeasureLabelsPerName != nil {
		t.Errorf("unset caps should stay nil, got %+v", c)
	}

	raw, _, err := store.CreateAPIKey(ctx, ten.ID, ScopePublicIngest)
	if err != nil {
		t.Fatal(err)
	}
	byKey, err := store.LookupByRawAPIKey(ctx, raw, ScopePublicIngest)
	if err != nil || byKey.Config.MetricCaps.MaxMetricDims == nil || *byKey.Config.MetricCaps.MaxMetricDims != 4 {
		t.Errorf("lookup by key: %+v err=%v, want the same overrides", byKey, err)
	}

	zero, tooMany := 0, 9
	for _, bad := range []MetricCaps{{MaxCounterNames: &zero}, {MaxMetricDims: &tooMany}} {
		if err := store.UpsertConfig(ctx, Config{TenantID: ten.ID, TierCeiling: "basic", TierEnforcementMode: "strip", MetricCaps: bad}); err == nil {
			t.Errorf("caps %+v should be refused", bad)
		}
	}
}
