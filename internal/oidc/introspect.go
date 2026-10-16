package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// maxIntrospectionBodyBytes caps the response we will read; a legitimate
	// RFC 7662 response is a few hundred bytes.
	maxIntrospectionBodyBytes = 64 << 10

	// maxIntrospectionCacheEntries bounds memory under a flood of distinct
	// tokens. When full of live entries, new results are simply not cached.
	maxIntrospectionCacheEntries = 10_000
)

// IntrospectionConfig configures RFC 7662 introspection of opaque tokens.
type IntrospectionConfig struct {
	Enabled bool
	// Endpoint overrides the discovery document's introspection_endpoint.
	Endpoint     string
	ClientID     string
	ClientSecret string
	// CacheTTL caps how long an active result is reused, which makes it the
	// worst-case revocation latency.
	CacheTTL time.Duration
	// NegativeCacheTTL is how long a rejection is remembered.
	NegativeCacheTTL time.Duration
}

// introspectionCache is keyed by sha256(token): the raw token is never stored.
type introspectionCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]introspectionEntry
}

type introspectionEntry struct {
	claims    jwt.MapClaims
	err       error // nil, or a cacheable rejection
	expiresAt time.Time
}

func (c *introspectionCache) get(key [sha256.Size]byte, now time.Time) (introspectionEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return introspectionEntry{}, false
	}
	if !now.Before(e.expiresAt) {
		delete(c.entries, key)
		return introspectionEntry{}, false
	}
	return e, true
}

func (c *introspectionCache) put(key [sha256.Size]byte, e introspectionEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[[sha256.Size]byte]introspectionEntry)
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxIntrospectionCacheEntries {
		for k, old := range c.entries {
			if !now.Before(old.expiresAt) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxIntrospectionCacheEntries {
			return
		}
	}
	c.entries[key] = e
}

// IntrospectionEnabled reports whether opaque tokens can be verified.
func (v *Verifier) IntrospectionEnabled() bool { return v.Introspection.Enabled }

// Introspect verifies an opaque access token with the provider (RFC 7662) and
// returns the introspection response as claims. Errors wrap exactly one of
// ErrInvalidToken, ErrInsufficientScope (*ScopeError) or ErrProviderUnavailable.
//
// Results are cached by sha256(token), so revocation at the provider only
// takes effect once the cached entry expires (Introspection.CacheTTL).
func (v *Verifier) Introspect(ctx context.Context, rawToken string) (jwt.MapClaims, error) {
	key := sha256.Sum256([]byte(rawToken))
	if e, ok := v.introCache.get(key, v.now()); ok {
		return e.claims, e.err
	}

	res, err, _ := v.introFlight.Do(hex.EncodeToString(key[:]), func() (any, error) {
		// A concurrent call may have filled the cache while we queued.
		if e, ok := v.introCache.get(key, v.now()); ok {
			return e, nil
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()

		resp, err := v.callIntrospection(ctx, rawToken)
		if err != nil && !errors.Is(err, ErrInvalidToken) {
			return nil, err // provider trouble is never cached
		}

		now := v.now()
		var e introspectionEntry
		if err == nil {
			e = v.validateIntrospection(resp, now)
		} else {
			e = introspectionEntry{err: err}
		}
		switch {
		case e.err != nil:
			e.expiresAt = now.Add(v.Introspection.NegativeCacheTTL)
		default:
			ttl := v.Introspection.CacheTTL
			if exp, err := e.claims.GetExpirationTime(); err == nil && exp != nil {
				ttl = min(ttl, exp.Sub(now))
			}
			e.expiresAt = now.Add(ttl)
		}
		if e.expiresAt.After(now) {
			v.introCache.put(key, e, now)
		}
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	e := res.(introspectionEntry)
	return e.claims, e.err
}

// callIntrospection POSTs the token per RFC 7662 §2.1 with client_secret_basic
// client authentication. It returns ErrInvalidToken only when the provider
// itself calls the token malformed (HTTP 400).
func (v *Verifier) callIntrospection(ctx context.Context, rawToken string) (map[string]any, error) {
	endpoint := v.Introspection.Endpoint
	if endpoint == "" {
		meta, err := v.discover(ctx)
		if err != nil {
			return nil, err
		}
		endpoint = meta.IntrospectionEndpoint
	}
	if endpoint == "" {
		return nil, unavailable(errors.New("no introspection endpoint configured or discovered"))
	}

	form := url.Values{"token": {rawToken}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, unavailable(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 6749 §2.3.1: the id and secret are form-urlencoded before Basic.
	req.SetBasicAuth(url.QueryEscape(v.Introspection.ClientID), url.QueryEscape(v.Introspection.ClientSecret))

	// Never follow a redirect with client credentials attached.
	client := *v.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.Do(req)
	if err != nil {
		// url.Error carries the endpoint URL, not the token or credentials.
		return nil, unavailable(fmt.Errorf("introspection request: %w", err))
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		return nil, invalidf("provider rejected the token as malformed")
	default:
		// Includes 401/403, i.e. our own client credentials being refused:
		// that is a server misconfiguration, not the caller's problem.
		return nil, unavailable(fmt.Errorf("introspection endpoint returned status %d", resp.StatusCode))
	}

	body, err := readCapped(resp.Body, maxIntrospectionBodyBytes)
	if err != nil {
		return nil, unavailable(fmt.Errorf("reading introspection response: %w", err))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, unavailable(fmt.Errorf("decoding introspection response: %w", err))
	}
	return out, nil
}

// validateIntrospection applies the acceptance rules to a decoded response.
func (v *Verifier) validateIntrospection(resp map[string]any, now time.Time) introspectionEntry {
	reject := func(err error) introspectionEntry { return introspectionEntry{err: err} }

	if active, _ := resp["active"].(bool); !active {
		return reject(invalidf("token is not active"))
	}
	claims := jwt.MapClaims(resp)

	// RFC 7662 makes iss optional; when the provider states one it must be ours.
	if _, present := claims["iss"]; present {
		iss, err := claims.GetIssuer()
		if err != nil || iss != v.IssuerURL {
			return reject(invalidf("issuer %q does not match", iss))
		}
	}

	// aud is optional in RFC 7662 but is what binds a token to this API, so
	// a response without it cannot be accepted.
	aud, err := claims.GetAudience()
	if err != nil || !containsString(aud, v.Audience) {
		return reject(invalidf("audience does not include %q", v.Audience))
	}

	if exp, err := claims.GetExpirationTime(); err != nil {
		return reject(invalidf("malformed exp: %v", err))
	} else if exp != nil && now.After(exp.Add(clockSkewLeeway)) {
		return reject(invalidf("token is expired"))
	}
	if nbf, err := claims.GetNotBefore(); err != nil {
		return reject(invalidf("malformed nbf: %v", err))
	} else if nbf != nil && now.Add(clockSkewLeeway).Before(nbf.Time) {
		return reject(invalidf("token is not valid yet"))
	}

	if err := v.checkScopes(claims); err != nil {
		return reject(err)
	}
	return introspectionEntry{claims: claims}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
