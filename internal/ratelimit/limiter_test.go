package ratelimit

import (
	"testing"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/tenant"
)

func TestLimiter_DefaultsAllowBurstThenBlock(t *testing.T) {
	l := New(Defaults{RequestsPerSecond: 1, BytesPerSecond: 1_000_000, Burst: 2})
	id := uuid.New()
	cfg := tenant.Config{}

	if !l.Allow(id, cfg, 10) {
		t.Fatalf("expected first request to be allowed")
	}
	if !l.Allow(id, cfg, 10) {
		t.Fatalf("expected second request (within burst) to be allowed")
	}
	if l.Allow(id, cfg, 10) {
		t.Fatalf("expected third request to exceed burst and be denied")
	}
}

func TestLimiter_PerTenantOverrideWins(t *testing.T) {
	l := New(Defaults{RequestsPerSecond: 1, BytesPerSecond: 1_000_000, Burst: 1})
	id := uuid.New()

	override := 100.0
	burst := 50
	cfg := tenant.Config{RateLimitRequestsPerSec: &override, RateLimitBurst: &burst}

	allowed := 0
	for i := 0; i < 10; i++ {
		if l.Allow(id, cfg, 1) {
			allowed++
		}
	}
	if allowed != 10 {
		t.Errorf("expected all 10 requests allowed under higher per-tenant override, got %d", allowed)
	}
}

func TestLimiter_BytesBucketBlocksOversizedRequest(t *testing.T) {
	l := New(Defaults{RequestsPerSecond: 1000, BytesPerSecond: 100, Burst: 1000})
	id := uuid.New()
	cfg := tenant.Config{}

	if l.Allow(id, cfg, 10_000) {
		t.Fatalf("expected an oversized single request to be denied by the bytes bucket")
	}
}

func TestLimiter_SeparateTenantsDoNotShareBuckets(t *testing.T) {
	l := New(Defaults{RequestsPerSecond: 1, BytesPerSecond: 1_000_000, Burst: 1})
	a, b := uuid.New(), uuid.New()
	cfg := tenant.Config{}

	if !l.Allow(a, cfg, 1) {
		t.Fatalf("tenant a's first request should be allowed")
	}
	if !l.Allow(b, cfg, 1) {
		t.Fatalf("tenant b's first request should be allowed independently of tenant a")
	}
}
