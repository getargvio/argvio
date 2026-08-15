package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Resolver is what the public/metrics servers depend on for API-key auth —
// deliberately an interface so a Redis-backed implementation can replace
// Cache later without touching call sites.
type Resolver interface {
	// Resolve returns (nil, false, nil) for a definitively-unknown/revoked
	// key (cached negative result), and a non-nil error only for
	// unexpected failures (e.g. Postgres unreachable) that callers should
	// treat as "fail closed, reject the request" rather than "not found".
	Resolve(ctx context.Context, rawKey, scope string) (*Resolved, bool, error)
}

// Cache is an in-memory, TTL-based Resolver in front of a Store. It caches
// both hits and misses (misses with a shorter TTL) so that unknown/revoked
// keys — including retry storms and scanning — don't hammer Postgres on
// the public server's hot path.
type Cache struct {
	store  *Store
	ttl    time.Duration
	negTTL time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	resolved  *Resolved
	found     bool
	expiresAt time.Time
}

// NewCache builds a Cache with the given positive-hit TTL. Negative results
// are cached for a quarter of that (min 1s), long enough to blunt a retry
// storm without meaningfully delaying a just-created key from working.
func NewCache(store *Store, ttl time.Duration) *Cache {
	negTTL := ttl / 4
	if negTTL < time.Second {
		negTTL = time.Second
	}
	return &Cache{
		store:   store,
		ttl:     ttl,
		negTTL:  negTTL,
		entries: make(map[string]cacheEntry),
	}
}

func cacheKey(rawKey, scope string) string {
	sum := sha256.Sum256([]byte(rawKey + "\x00" + scope))
	return hex.EncodeToString(sum[:])
}

// Resolve implements Resolver.
func (c *Cache) Resolve(ctx context.Context, rawKey, scope string) (*Resolved, bool, error) {
	key := cacheKey(rawKey, scope)

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.resolved, entry.found, nil
	}

	resolved, err := c.store.LookupByRawAPIKey(ctx, rawKey, scope)
	switch {
	case err == nil:
		c.set(key, cacheEntry{resolved: resolved, found: true, expiresAt: time.Now().Add(c.ttl)})
		return resolved, true, nil
	case isNotFound(err):
		c.set(key, cacheEntry{found: false, expiresAt: time.Now().Add(c.negTTL)})
		return nil, false, nil
	default:
		// Do not cache infrastructure failures — a transient Postgres
		// blip must not turn into a sticky false rejection.
		return nil, false, err
	}
}

func (c *Cache) set(key string, e cacheEntry) {
	c.mu.Lock()
	c.entries[key] = e
	c.mu.Unlock()
}

func isNotFound(err error) bool {
	return err == ErrNotFound
}

// Janitor periodically evicts expired entries so long-running processes
// under scan/attack traffic don't grow the map unboundedly. Run it in its
// own goroutine; it returns when stopCh is closed.
func (c *Cache) Janitor(interval time.Duration, stopCh <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stopCh:
			return
		case now := <-t.C:
			c.mu.Lock()
			for k, e := range c.entries {
				if now.After(e.expiresAt) {
					delete(c.entries, k)
				}
			}
			c.mu.Unlock()
		}
	}
}
