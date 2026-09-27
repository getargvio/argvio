package ratelimit

import (
	"testing"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/tenant"
)

func identityFor(tenantID uuid.UUID, method authn.Method, cfg tenant.Config) authn.Identity {
	cfg.TenantID = tenantID
	return authn.Identity{
		Method: method,
		Resolved: &tenant.Resolved{
			Tenant: tenant.Tenant{ID: tenantID, Status: "active"},
			Config: cfg,
		},
	}
}

func TestLimiter_DefaultsAllowBurstThenBlock(t *testing.T) {
	l := New(Defaults{RequestsPerSecond: 1, BytesPerSecond: 1_000_000, Burst: 2}, nil)
	id := identityFor(uuid.New(), authn.MethodAPIKey, tenant.Config{})

	if !l.Allow(id, 10) {
		t.Fatalf("expected first request to be allowed")
	}
	if !l.Allow(id, 10) {
		t.Fatalf("expected second request (within burst) to be allowed")
	}
	if l.Allow(id, 10) {
		t.Fatalf("expected third request to exceed burst and be denied")
	}
}

func TestLimiter_PerTenantOverrideWins(t *testing.T) {
	l := New(Defaults{RequestsPerSecond: 1, BytesPerSecond: 1_000_000, Burst: 1}, nil)

	override := 100.0
	burst := 50
	id := identityFor(uuid.New(), authn.MethodAPIKey, tenant.Config{RateLimitRequestsPerSec: &override, RateLimitBurst: &burst})

	allowed := 0
	for i := 0; i < 10; i++ {
		if l.Allow(id, 1) {
			allowed++
		}
	}
	if allowed != 10 {
		t.Errorf("expected all 10 requests allowed under higher per-tenant override, got %d", allowed)
	}
}

func TestLimiter_BytesBucketBlocksOversizedRequest(t *testing.T) {
	l := New(Defaults{RequestsPerSecond: 1000, BytesPerSecond: 100, Burst: 1000}, nil)
	id := identityFor(uuid.New(), authn.MethodAPIKey, tenant.Config{})

	if l.Allow(id, 10_000) {
		t.Fatalf("expected an oversized single request to be denied by the bytes bucket")
	}
}

func TestLimiter_SeparateTenantsDoNotShareBuckets(t *testing.T) {
	l := New(Defaults{RequestsPerSecond: 1, BytesPerSecond: 1_000_000, Burst: 1}, nil)
	a := identityFor(uuid.New(), authn.MethodAPIKey, tenant.Config{})
	b := identityFor(uuid.New(), authn.MethodAPIKey, tenant.Config{})

	if !l.Allow(a, 1) {
		t.Fatalf("tenant a's first request should be allowed")
	}
	if !l.Allow(b, 1) {
		t.Fatalf("tenant b's first request should be allowed independently of tenant a")
	}
}

func TestLimiter_PerMethodDefaultsApplyIndependently(t *testing.T) {
	l := New(
		Defaults{RequestsPerSecond: 1, BytesPerSecond: 1_000_000, Burst: 1},
		map[authn.Method]Defaults{authn.MethodJWT: {RequestsPerSecond: 100, BytesPerSecond: 1_000_000, Burst: 100}},
	)
	tenantID := uuid.New()
	apiKeyID := identityFor(tenantID, authn.MethodAPIKey, tenant.Config{})
	jwtID := identityFor(tenantID, authn.MethodJWT, tenant.Config{})

	// Exhaust the api_key bucket (burst 1).
	if !l.Allow(apiKeyID, 1) {
		t.Fatalf("api_key identity's first request should be allowed")
	}
	if l.Allow(apiKeyID, 1) {
		t.Fatalf("api_key identity's second request should exceed its burst of 1")
	}

	// JWT bucket is independent of the exhausted api_key bucket.
	allowed := 0
	for i := 0; i < 10; i++ {
		if l.Allow(jwtID, 1) {
			allowed++
		}
	}
	if allowed != 10 {
		t.Errorf("expected all 10 jwt-identity requests allowed under its own per-method defaults, got %d", allowed)
	}
}
