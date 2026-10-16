package oidc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const opaqueToken = "opaque-access-token-0123456789"

func TestIntrospect_Outcomes(t *testing.T) {
	tests := []struct {
		name    string
		handler func(p *testProvider) func(*http.Request, string) (int, any)
		want    error // nil = accepted
	}{
		{"active", func(p *testProvider) func(*http.Request, string) (int, any) { return activeResponse(p, nil) }, nil},
		{"inactive", func(p *testProvider) func(*http.Request, string) (int, any) {
			return func(*http.Request, string) (int, any) { return 200, map[string]any{"active": false} }
		}, ErrInvalidToken},
		{"active missing", func(p *testProvider) func(*http.Request, string) (int, any) {
			return func(*http.Request, string) (int, any) { return 200, map[string]any{"sub": "x"} }
		}, ErrInvalidToken},
		{"active not a bool", activeMutated(func(m map[string]any) { m["active"] = "true" }), ErrInvalidToken},
		{"expired", activeMutated(func(m map[string]any) { m["exp"] = time.Now().Add(-time.Hour).Unix() }), ErrInvalidToken},
		{"not yet valid", activeMutated(func(m map[string]any) { m["nbf"] = time.Now().Add(time.Hour).Unix() }), ErrInvalidToken},
		{"wrong audience", activeMutated(func(m map[string]any) { m["aud"] = "https://other.example.com" }), ErrInvalidToken},
		{"missing audience", activeMutated(func(m map[string]any) { delete(m, "aud") }), ErrInvalidToken},
		{"audience list containing ours", activeMutated(func(m map[string]any) { m["aud"] = []any{"other", testAudience} }), nil},
		{"wrong issuer", activeMutated(func(m map[string]any) { m["iss"] = "https://evil.example.com" }), ErrInvalidToken},
		{"issuer absent is tolerated", activeMutated(func(m map[string]any) { delete(m, "iss") }), nil},
		{"no exp is tolerated", activeMutated(func(m map[string]any) { delete(m, "exp") }), nil},
		{"provider calls token malformed", func(p *testProvider) func(*http.Request, string) (int, any) {
			return func(*http.Request, string) (int, any) { return 400, `{"error":"invalid_request"}` }
		}, ErrInvalidToken},
		{"provider 500", func(p *testProvider) func(*http.Request, string) (int, any) {
			return func(*http.Request, string) (int, any) { return 500, `oops` }
		}, ErrProviderUnavailable},
		{"our client credentials refused", func(p *testProvider) func(*http.Request, string) (int, any) {
			return func(*http.Request, string) (int, any) { return 401, `{"error":"invalid_client"}` }
		}, ErrProviderUnavailable},
		{"non-JSON 200", func(p *testProvider) func(*http.Request, string) (int, any) {
			return func(*http.Request, string) (int, any) { return 200, `<html>login</html>` }
		}, ErrProviderUnavailable},
		{"oversized response", func(p *testProvider) func(*http.Request, string) (int, any) {
			return func(*http.Request, string) (int, any) {
				return 200, `{"active":true,"pad":"` + strings.Repeat("a", maxIntrospectionBodyBytes) + `"}`
			}
		}, ErrProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t)
			p.setIntrospect(tt.handler(p))
			claims, err := p.newVerifier().Introspect(context.Background(), opaqueToken)
			switch {
			case tt.want == nil && err != nil:
				t.Fatalf("err = %v, want accepted", err)
			case tt.want == nil && claims["sub"] != "user-1" && claims["sub"] != "x":
				t.Errorf("claims = %v", claims)
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func activeMutated(mutate func(map[string]any)) func(p *testProvider) func(*http.Request, string) (int, any) {
	return func(p *testProvider) func(*http.Request, string) (int, any) { return activeResponse(p, mutate) }
}

func TestIntrospect_RequestFormat(t *testing.T) {
	p := newTestProvider(t)
	p.setIntrospect(activeResponse(p, nil))
	v := p.newVerifier()
	v.Introspection.ClientID = "my client"
	v.Introspection.ClientSecret = "p@ss:word/+"

	if _, err := v.Introspect(context.Background(), opaqueToken); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastReq.Method != http.MethodPost {
		t.Errorf("method = %s", p.lastReq.Method)
	}
	id, secret, ok := p.lastReq.BasicAuth()
	if !ok || id != "my+client" || secret != "p%40ss%3Aword%2F%2B" {
		t.Errorf("basic auth = %q / %q, want form-urlencoded per RFC 6749 §2.3.1", id, secret)
	}
	if p.lastForm["token"] != opaqueToken || p.lastForm["hint"] != "access_token" {
		t.Errorf("form = %v", p.lastForm)
	}
}

func TestIntrospect_EndpointOverride(t *testing.T) {
	p := newTestProvider(t, withoutIntrospection)
	p.setIntrospect(activeResponse(p, nil))
	v := p.newVerifier()

	if _, err := v.Introspect(context.Background(), opaqueToken); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("without any endpoint: err = %v, want ErrProviderUnavailable", err)
	}
	v.Introspection.Endpoint = p.srv.URL + "/introspect"
	if _, err := v.Introspect(context.Background(), opaqueToken); err != nil {
		t.Fatalf("with configured endpoint: %v", err)
	}
}

func TestIntrospect_Cache(t *testing.T) {
	ctx := context.Background()

	t.Run("hit does not call the provider again", func(t *testing.T) {
		p := newTestProvider(t)
		p.setIntrospect(activeResponse(p, nil))
		v := p.newVerifier()
		for i := 0; i < 5; i++ {
			if _, err := v.Introspect(ctx, opaqueToken); err != nil {
				t.Fatal(err)
			}
		}
		if got := p.introspectHits.Load(); got != 1 {
			t.Errorf("introspection calls = %d, want 1", got)
		}
	})

	t.Run("cached entries are keyed by sha256, never the raw token", func(t *testing.T) {
		p := newTestProvider(t)
		p.setIntrospect(activeResponse(p, nil))
		v := p.newVerifier()
		if _, err := v.Introspect(ctx, opaqueToken); err != nil {
			t.Fatal(err)
		}
		v.introCache.mu.Lock()
		defer v.introCache.mu.Unlock()
		if len(v.introCache.entries) != 1 {
			t.Fatalf("entries = %d", len(v.introCache.entries))
		}
		for k := range v.introCache.entries {
			if strings.Contains(string(k[:]), opaqueToken) {
				t.Error("raw token found in cache key")
			}
		}
	})

	t.Run("positive entry expires after cache_ttl", func(t *testing.T) {
		p := newTestProvider(t)
		p.setIntrospect(activeResponse(p, nil))
		v := p.newVerifier()
		now := time.Now()
		v.now = func() time.Time { return now }
		_, _ = v.Introspect(ctx, opaqueToken)
		now = now.Add(v.Introspection.CacheTTL - time.Second)
		_, _ = v.Introspect(ctx, opaqueToken)
		if got := p.introspectHits.Load(); got != 1 {
			t.Fatalf("calls before TTL = %d, want 1", got)
		}
		now = now.Add(2 * time.Second)
		_, _ = v.Introspect(ctx, opaqueToken)
		if got := p.introspectHits.Load(); got != 2 {
			t.Errorf("calls after TTL = %d, want 2", got)
		}
	})

	t.Run("positive TTL is capped by token exp", func(t *testing.T) {
		p := newTestProvider(t)
		now := time.Now()
		p.setIntrospect(activeResponse(p, func(m map[string]any) { m["exp"] = now.Add(10 * time.Second).Unix() }))
		v := p.newVerifier() // cache_ttl is 1m
		v.now = func() time.Time { return now }
		_, _ = v.Introspect(ctx, opaqueToken)
		now = now.Add(12 * time.Second) // past exp, inside cache_ttl, inside leeway
		_, _ = v.Introspect(ctx, opaqueToken)
		if got := p.introspectHits.Load(); got != 2 {
			t.Errorf("calls = %d, want 2: entry must not outlive exp", got)
		}
	})

	t.Run("rejections are cached briefly", func(t *testing.T) {
		p := newTestProvider(t) // default handler answers active:false
		v := p.newVerifier()
		now := time.Now()
		v.now = func() time.Time { return now }
		for i := 0; i < 3; i++ {
			if _, err := v.Introspect(ctx, opaqueToken); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v", err)
			}
		}
		if got := p.introspectHits.Load(); got != 1 {
			t.Fatalf("calls within negative TTL = %d, want 1", got)
		}
		now = now.Add(v.Introspection.NegativeCacheTTL + time.Second)
		_, _ = v.Introspect(ctx, opaqueToken)
		if got := p.introspectHits.Load(); got != 2 {
			t.Errorf("calls after negative TTL = %d, want 2", got)
		}
	})

	t.Run("provider failures are never cached", func(t *testing.T) {
		p := newTestProvider(t)
		p.setIntrospect(activeResponse(p, nil))
		v := p.newVerifier()
		p.introspectStatus.Store(503)
		if _, err := v.Introspect(ctx, opaqueToken); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("err = %v", err)
		}
		p.introspectStatus.Store(0)
		if _, err := v.Introspect(ctx, opaqueToken); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	})

	t.Run("insufficient scope is cached and stays a scope error", func(t *testing.T) {
		p := newTestProvider(t)
		p.setIntrospect(activeResponse(p, func(m map[string]any) { m["scope"] = "openid" }))
		v := p.newVerifier()
		v.RequiredScopes = []string{"metrics:read"}
		for i := 0; i < 2; i++ {
			if _, err := v.Introspect(ctx, opaqueToken); !errors.Is(err, ErrInsufficientScope) {
				t.Fatalf("err = %v, want ErrInsufficientScope", err)
			}
		}
		if got := p.introspectHits.Load(); got != 1 {
			t.Errorf("calls = %d, want 1", got)
		}
	})

	t.Run("cache is bounded", func(t *testing.T) {
		var c introspectionCache
		now := time.Now()
		for i := 0; i < maxIntrospectionCacheEntries+50; i++ {
			c.put([32]byte{byte(i), byte(i >> 8), byte(i >> 16)}, introspectionEntry{expiresAt: now.Add(time.Hour)}, now)
		}
		if len(c.entries) > maxIntrospectionCacheEntries {
			t.Errorf("entries = %d, want <= %d", len(c.entries), maxIntrospectionCacheEntries)
		}
	})
}

func TestIntrospect_ConcurrentLookupsShareOneCall(t *testing.T) {
	p := newTestProvider(t)
	release := make(chan struct{})
	p.setIntrospect(func(r *http.Request, token string) (int, any) {
		<-release
		return activeResponse(p, nil)(r, token)
	})
	v := p.newVerifier()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Introspect(context.Background(), opaqueToken)
			errs <- err
		}()
	}
	// Give every goroutine time to join the in-flight call before it returns.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := p.introspectHits.Load(); got != 1 {
		t.Errorf("introspection calls = %d, want 1", got)
	}
}

func TestIntrospect_DoesNotFollowRedirects(t *testing.T) {
	p := newTestProvider(t)
	var leaked bool
	p.setIntrospect(func(*http.Request, string) (int, any) { return 200, map[string]any{"active": true} })
	// Point at an endpoint that redirects; the redirect target must never be hit.
	target := newTestProvider(t)
	target.setIntrospect(func(r *http.Request, _ string) (int, any) {
		leaked = true
		return 200, map[string]any{"active": true}
	})
	redirector := http.NewServeMux()
	redirector.HandleFunc("/introspect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+"/introspect", http.StatusTemporaryRedirect)
	})
	srv := newServer(t, redirector)
	v := p.newVerifier()
	v.Introspection.Endpoint = srv + "/introspect"

	if _, err := v.Introspect(context.Background(), opaqueToken); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if leaked {
		t.Error("introspection request with client credentials followed a redirect")
	}
}
