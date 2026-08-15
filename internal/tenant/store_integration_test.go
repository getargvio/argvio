package tenant

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/storage"
)

// TestStoreAndCache_Live exercises tenant creation, API-key issuance,
// lookup, cache hit/negative-hit behavior, and revocation against a real
// Postgres/Timescale instance (same gating as internal/storage's live test).
func TestStoreAndCache_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live tenant store test")
	}
	ctx := context.Background()

	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	t.Cleanup(func() { _ = storage.MigrateDown(dsn) })

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	store := NewStore(pool)

	tenantSlug := "test-tenant-" + time.Now().Format("150405.000000")
	ten, err := store.CreateTenant(ctx, tenantSlug, "Test Tenant")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	rateLimit := 42.0
	if err := store.UpsertConfig(ctx, Config{
		TenantID:                ten.ID,
		TierCeiling:             "full",
		TierEnforcementMode:     "strip",
		RateLimitRequestsPerSec: &rateLimit,
	}); err != nil {
		t.Fatalf("UpsertConfig: %v", err)
	}

	raw, key, err := store.CreateAPIKey(ctx, ten.ID, ScopePublicIngest)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if raw == "" || key.ID.String() == "" {
		t.Fatalf("expected non-empty raw key and key ID")
	}

	resolved, err := store.LookupByRawAPIKey(ctx, raw, ScopePublicIngest)
	if err != nil {
		t.Fatalf("LookupByRawAPIKey: %v", err)
	}
	if resolved.Tenant.ID != ten.ID {
		t.Errorf("resolved tenant ID = %v, want %v", resolved.Tenant.ID, ten.ID)
	}
	if resolved.Config.TierCeiling != "full" {
		t.Errorf("tier ceiling = %q, want full", resolved.Config.TierCeiling)
	}
	if resolved.Config.RateLimitRequestsPerSec == nil || *resolved.Config.RateLimitRequestsPerSec != 42.0 {
		t.Errorf("rate limit override not round-tripped correctly")
	}
	if !resolved.Active() {
		t.Errorf("expected resolved tenant/key to be active")
	}

	// Wrong scope should not resolve.
	if _, err := store.LookupByRawAPIKey(ctx, raw, ScopeMetricsQuery); err != ErrNotFound {
		t.Errorf("expected ErrNotFound for wrong scope, got %v", err)
	}

	cache := NewCache(store, 200*time.Millisecond)

	r1, found, err := cache.Resolve(ctx, raw, ScopePublicIngest)
	if err != nil || !found || r1.Tenant.ID != ten.ID {
		t.Fatalf("cache.Resolve (cold): found=%v err=%v", found, err)
	}
	r2, found, err := cache.Resolve(ctx, raw, ScopePublicIngest)
	if err != nil || !found || r2.Tenant.ID != ten.ID {
		t.Fatalf("cache.Resolve (warm): found=%v err=%v", found, err)
	}

	_, found, err = cache.Resolve(ctx, "argv_live_pub_totally-bogus-key", ScopePublicIngest)
	if err != nil || found {
		t.Fatalf("expected negative cache result for bogus key: found=%v err=%v", found, err)
	}

	if err := store.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, err := store.LookupByRawAPIKey(ctx, raw, ScopePublicIngest); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after revocation, got %v", err)
	}
}
