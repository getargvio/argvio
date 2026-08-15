// Package ratelimit provides per-tenant request/byte rate limiting for the
// public server. Each tenant gets its own token bucket pair (requests/sec,
// bytes/sec) so one noisy or hostile tenant can't starve others — sized
// from the tenant's Config override if set, otherwise the server-wide
// default from internal/config.PublicConfig.
package ratelimit

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/getargvio/argvio/internal/tenant"
)

// Defaults are the server-wide fallback limits, used whenever a tenant's
// Config doesn't override them.
type Defaults struct {
	RequestsPerSecond float64
	BytesPerSecond    float64
	Burst             int
}

type bucket struct {
	requests *rate.Limiter
	bytes    *rate.Limiter
}

// Limiter holds one bucket pair per tenant, created lazily on first use.
type Limiter struct {
	defaults Defaults

	mu      sync.Mutex
	buckets map[uuid.UUID]*bucket
}

func New(defaults Defaults) *Limiter {
	return &Limiter{
		defaults: defaults,
		buckets:  make(map[uuid.UUID]*bucket),
	}
}

// Allow reports whether a request of nBytes for the given tenant may
// proceed right now, consuming from both the request-count and byte-count
// buckets. It never blocks — the public server must fail fast (429) rather
// than queue, since ingest is the hot path.
func (l *Limiter) Allow(tenantID uuid.UUID, cfg tenant.Config, nBytes int) bool {
	b := l.bucketFor(tenantID, cfg)
	// Check-then-consume on both; if the byte check fails after the request
	// token was already spent, that request-token is simply lost this tick
	// — acceptable, since under sustained overload both buckets will be
	// empty anyway and the tenant is rate-limited regardless.
	if !b.requests.Allow() {
		return false
	}
	return b.bytes.AllowN(time.Now(), nBytes)
}

func (l *Limiter) bucketFor(tenantID uuid.UUID, cfg tenant.Config) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()

	if b, ok := l.buckets[tenantID]; ok {
		return b
	}

	rps := l.defaults.RequestsPerSecond
	if cfg.RateLimitRequestsPerSec != nil {
		rps = *cfg.RateLimitRequestsPerSec
	}
	bps := l.defaults.BytesPerSecond
	if cfg.RateLimitBytesPerSec != nil {
		bps = *cfg.RateLimitBytesPerSec
	}
	burst := l.defaults.Burst
	if cfg.RateLimitBurst != nil {
		burst = *cfg.RateLimitBurst
	}

	// Byte-bucket burst: one second's worth of sustained throughput, the
	// standard token-bucket sizing when there's no independently-configured
	// burst for bytes (unlike requests, which has its own explicit burst
	// knob since request bursts and byte bursts don't scale together).
	byteBurst := int(bps)
	if byteBurst <= 0 {
		byteBurst = 1
	}

	b := &bucket{
		requests: rate.NewLimiter(rate.Limit(rps), burst),
		bytes:    rate.NewLimiter(rate.Limit(bps), byteBurst),
	}
	l.buckets[tenantID] = b
	return b
}
