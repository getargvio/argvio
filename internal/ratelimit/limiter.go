// Package ratelimit provides per-tenant, per-auth-method request/byte rate
// limiting for the public server.
package ratelimit

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/getargvio/argvio/internal/authn"
)

type Defaults struct {
	RequestsPerSecond float64
	BytesPerSecond    float64
	Burst             int
}

func (d Defaults) zero() bool {
	return d.RequestsPerSecond == 0 && d.BytesPerSecond == 0 && d.Burst == 0
}

type bucket struct {
	requests *rate.Limiter
	bytes    *rate.Limiter
}

type bucketKey struct {
	tenantID uuid.UUID
	method   authn.Method
}

type Limiter struct {
	defaults  Defaults
	perMethod map[authn.Method]Defaults

	mu      sync.Mutex
	buckets map[bucketKey]*bucket
}

// New builds a Limiter; perMethod overrides defaults per auth method.
func New(defaults Defaults, perMethod map[authn.Method]Defaults) *Limiter {
	return &Limiter{
		defaults:  defaults,
		perMethod: perMethod,
		buckets:   make(map[bucketKey]*bucket),
	}
}

// Allow never blocks; it fails fast so ingest isn't queued.
func (l *Limiter) Allow(id authn.Identity, nBytes int) bool {
	b := l.bucketFor(id)
	if !b.requests.Allow() {
		return false
	}
	return b.bytes.AllowN(time.Now(), nBytes)
}

func (l *Limiter) bucketFor(id authn.Identity) *bucket {
	key := bucketKey{tenantID: id.Resolved.Tenant.ID, method: id.Method}

	l.mu.Lock()
	defer l.mu.Unlock()

	if b, ok := l.buckets[key]; ok {
		return b
	}

	defaults := l.defaults
	if d, ok := l.perMethod[id.Method]; ok && !d.zero() {
		defaults = d
	}

	cfg := id.Resolved.Config
	rps := defaults.RequestsPerSecond
	if cfg.RateLimitRequestsPerSec != nil {
		rps = *cfg.RateLimitRequestsPerSec
	}
	bps := defaults.BytesPerSecond
	if cfg.RateLimitBytesPerSec != nil {
		bps = *cfg.RateLimitBytesPerSec
	}
	burst := defaults.Burst
	if cfg.RateLimitBurst != nil {
		burst = *cfg.RateLimitBurst
	}

	byteBurst := int(bps) // no separate burst knob for bytes

	if byteBurst <= 0 {
		byteBurst = 1
	}

	b := &bucket{
		requests: rate.NewLimiter(rate.Limit(rps), burst),
		bytes:    rate.NewLimiter(rate.Limit(bps), byteBurst),
	}
	l.buckets[key] = b
	return b
}
