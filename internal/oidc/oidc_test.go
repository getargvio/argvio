package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testAudience = "https://api.example.com"

// testProvider is a fake OIDC provider: discovery, JWKS and an RFC 7662
// introspection endpoint, each with a request counter and an injectable
// status, backed by one RSA and one EC key.
type testProvider struct {
	srv *httptest.Server

	rsaKey *rsa.PrivateKey
	rsaKID string
	ecKey  *ecdsa.PrivateKey
	ecKID  string

	trailingSlash bool
	noIntrospect  bool // omit introspection_endpoint from discovery

	discoveryHits, jwksHits, introspectHits atomic.Int32
	discoveryStatus, jwksStatus             atomic.Int32
	introspectStatus                        atomic.Int32

	mu         sync.Mutex
	introspect func(r *http.Request, token string) (int, any)
	lastReq    *http.Request
	lastForm   map[string]string
	extraKeys  []map[string]any
}

func newTestProvider(t *testing.T, opts ...func(*testProvider)) *testProvider {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &testProvider{rsaKey: rk, rsaKID: "rsa-1", ecKey: ek, ecKID: "ec-1"}
	for _, o := range opts {
		o(p)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		p.discoveryHits.Add(1)
		if st := int(p.discoveryStatus.Load()); st != 0 {
			http.Error(w, "boom", st)
			return
		}
		doc := map[string]any{"issuer": p.issuer(), "jwks_uri": p.srv.URL + "/jwks.json"}
		if !p.noIntrospect {
			doc["introspection_endpoint"] = p.srv.URL + "/introspect"
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		p.jwksHits.Add(1)
		if st := int(p.jwksStatus.Load()); st != 0 {
			http.Error(w, "boom", st)
			return
		}
		p.mu.Lock()
		keys := append([]map[string]any{p.rsaJWK(), p.ecJWK()}, p.extraKeys...)
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	mux.HandleFunc("/introspect", func(w http.ResponseWriter, r *http.Request) {
		p.introspectHits.Add(1)
		_ = r.ParseForm()
		p.mu.Lock()
		p.lastReq = r
		p.lastForm = map[string]string{"token": r.PostForm.Get("token"), "hint": r.PostForm.Get("token_type_hint")}
		fn := p.introspect
		p.mu.Unlock()
		if st := int(p.introspectStatus.Load()); st != 0 {
			http.Error(w, "boom", st)
			return
		}
		if fn == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
			return
		}
		st, body := fn(r, r.PostForm.Get("token"))
		w.WriteHeader(st)
		if s, ok := body.(string); ok {
			_, _ = w.Write([]byte(s))
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func withTrailingSlash(p *testProvider)    { p.trailingSlash = true }
func withoutIntrospection(p *testProvider) { p.noIntrospect = true }

func (p *testProvider) issuer() string {
	if p.trailingSlash {
		return p.srv.URL + "/"
	}
	return p.srv.URL
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (p *testProvider) rsaJWK() map[string]any {
	e := p.rsaKey.PublicKey.E
	return map[string]any{
		"kty": "RSA", "kid": p.rsaKID, "use": "sig",
		"n": b64(p.rsaKey.PublicKey.N.Bytes()),
		"e": b64([]byte{byte(e >> 16), byte(e >> 8), byte(e)}),
	}
}

func (p *testProvider) ecJWK() map[string]any {
	return map[string]any{
		"kty": "EC", "crv": "P-256", "kid": p.ecKID, "use": "sig",
		"x": b64(p.ecKey.PublicKey.X.FillBytes(make([]byte, 32))),
		"y": b64(p.ecKey.PublicKey.Y.FillBytes(make([]byte, 32))),
	}
}

func (p *testProvider) claims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   p.issuer(),
		"aud":   testAudience,
		"sub":   "user-1",
		"scope": "openid metrics:read",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
}

// signRS signs with the RSA key; header overrides are merged over kid/typ.
func (p *testProvider) signRS(t *testing.T, claims jwt.MapClaims, header map[string]any) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = p.rsaKID
	for k, v := range header {
		if v == nil {
			delete(tok.Header, k)
			continue
		}
		tok.Header[k] = v
	}
	s, err := tok.SignedString(p.rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (p *testProvider) signES(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = p.ecKID
	s, err := tok.SignedString(p.ecKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (p *testProvider) newVerifier() *Verifier {
	v := NewVerifier(p.issuer(), testAudience, nil, time.Hour)
	v.Introspection = IntrospectionConfig{
		Enabled: true, ClientID: "argvio", ClientSecret: "s3cret",
		CacheTTL: time.Minute, NegativeCacheTTL: 5 * time.Second,
	}
	return v
}

// setIntrospect installs a handler answering every introspection call.
func (p *testProvider) setIntrospect(fn func(r *http.Request, token string) (int, any)) {
	p.mu.Lock()
	p.introspect = fn
	p.mu.Unlock()
}

func activeResponse(p *testProvider, mutate func(map[string]any)) func(*http.Request, string) (int, any) {
	return func(*http.Request, string) (int, any) {
		resp := map[string]any{
			"active": true, "iss": p.issuer(), "aud": testAudience, "sub": "user-1",
			"scope": "openid metrics:read", "exp": time.Now().Add(time.Hour).Unix(),
		}
		if mutate != nil {
			mutate(resp)
		}
		return http.StatusOK, resp
	}
}

func TestVerify_ValidTokens(t *testing.T) {
	for _, slash := range []bool{false, true} {
		name := "issuer without trailing slash"
		var opts []func(*testProvider)
		if slash {
			name = "issuer with trailing slash"
			opts = append(opts, withTrailingSlash)
		}
		t.Run(name, func(t *testing.T) {
			p := newTestProvider(t, opts...)
			v := p.newVerifier()

			for alg, token := range map[string]string{
				"RS256": p.signRS(t, p.claims(), nil),
				"ES256": p.signES(t, p.claims()),
			} {
				claims, err := v.Verify(context.Background(), token)
				if err != nil {
					t.Fatalf("%s: Verify: %v", alg, err)
				}
				if claims["sub"] != "user-1" {
					t.Errorf("%s: sub = %v", alg, claims["sub"])
				}
			}
		})
	}
}

func TestVerify_Rejections(t *testing.T) {
	p := newTestProvider(t)
	v := p.newVerifier()

	hmacTok := func() string {
		// Classic algorithm confusion: HS256 keyed with the RSA public key.
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, p.claims())
		tok.Header["kid"] = p.rsaKID
		s, err := tok.SignedString(p.rsaKey.PublicKey.N.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	noneTok := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, p.claims())
		s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	with := func(k string, val any) string {
		c := p.claims()
		if val == nil {
			delete(c, k)
		} else {
			c[k] = val
		}
		return p.signRS(t, c, nil)
	}

	tests := []struct {
		name  string
		token string
	}{
		{"wrong issuer", with("iss", "https://evil.example.com")},
		{"wrong audience", with("aud", "https://someone-else.example.com")},
		{"missing audience", with("aud", nil)},
		{"expired", with("exp", time.Now().Add(-time.Hour).Unix())},
		{"missing exp", with("exp", nil)},
		{"not yet valid", with("nbf", time.Now().Add(time.Hour).Unix())},
		{"alg none", noneTok()},
		{"HS256 signed with the public key", hmacTok()},
		{"signed by a different key", func() string {
			other, _ := rsa.GenerateKey(rand.Reader, 2048)
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, p.claims())
			tok.Header["kid"] = p.rsaKID
			s, _ := tok.SignedString(other)
			return s
		}()},
		{"garbage", "not.a.jwt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), tt.token)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerify_IDTokenRejected(t *testing.T) {
	p := newTestProvider(t)

	// An ID token's audience is the SPA's client_id, never the API's.
	idToken := func(typ string) string {
		c := p.claims()
		c["aud"] = "spa-client-id"
		c["nonce"] = "n-0S6_WzA2Mj"
		c["at_hash"] = "77QmUPtjPfzWtF2AnpK9RQ"
		var h map[string]any
		if typ != "" {
			h = map[string]any{"typ": typ}
		}
		return p.signRS(t, c, h)
	}

	t.Run("rejected on audience alone", func(t *testing.T) {
		v := p.newVerifier()
		if _, err := v.Verify(context.Background(), idToken("")); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("typ is enforced when required", func(t *testing.T) {
		v := p.newVerifier()
		v.RequireAccessTokenTyp = true
		// Even carrying the right audience, a typ other than at+jwt is out.
		c := p.claims()
		if _, err := v.Verify(context.Background(), p.signRS(t, c, map[string]any{"typ": "JWT"})); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("typ=JWT: err = %v, want ErrInvalidToken", err)
		}
		for _, typ := range []string{"at+jwt", "AT+JWT", "application/at+jwt"} {
			if _, err := v.Verify(context.Background(), p.signRS(t, c, map[string]any{"typ": typ})); err != nil {
				t.Errorf("typ=%q: %v", typ, err)
			}
		}
		// No typ header at all is tolerated (the audience check still applies).
		if _, err := v.Verify(context.Background(), p.signRS(t, c, map[string]any{"typ": nil})); err != nil {
			t.Errorf("no typ: %v", err)
		}
	})

	t.Run("typ is not enforced by default", func(t *testing.T) {
		v := p.newVerifier()
		if _, err := v.Verify(context.Background(), p.signRS(t, p.claims(), map[string]any{"typ": "JWT"})); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
}

func TestVerify_RequiredScopes(t *testing.T) {
	p := newTestProvider(t)
	v := p.newVerifier()
	v.RequiredScopes = []string{"metrics:read"}

	t.Run("missing scope is ErrInsufficientScope, not ErrInvalidToken", func(t *testing.T) {
		c := p.claims()
		c["scope"] = "openid"
		_, err := v.Verify(context.Background(), p.signRS(t, c, nil))
		if !errors.Is(err, ErrInsufficientScope) || errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInsufficientScope only", err)
		}
		var se *ScopeError
		if !errors.As(err, &se) || len(se.Missing) != 1 || se.Missing[0] != "metrics:read" {
			t.Errorf("ScopeError = %+v", se)
		}
	})
	t.Run("scope claim", func(t *testing.T) {
		if _, err := v.Verify(context.Background(), p.signRS(t, p.claims(), nil)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("scp claim", func(t *testing.T) {
		c := p.claims()
		delete(c, "scope")
		c["scp"] = []any{"metrics:read"}
		if _, err := v.Verify(context.Background(), p.signRS(t, c, nil)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestVerify_KeyRotation(t *testing.T) {
	p := newTestProvider(t)
	v := p.newVerifier()
	ctx := context.Background()

	if _, err := v.Verify(ctx, p.signRS(t, p.claims(), nil)); err != nil {
		t.Fatal(err)
	}
	if got := p.jwksHits.Load(); got != 1 {
		t.Fatalf("initial JWKS fetches = %d, want 1", got)
	}

	// The provider rotates in a new key well inside the cache TTL.
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	p.rsaKey, p.rsaKID = newKey, "rsa-2"
	rotated := p.signRS(t, p.claims(), nil)

	if _, err := v.Verify(ctx, rotated); err != nil {
		t.Fatalf("Verify after rotation: %v", err)
	}
	if got := p.jwksHits.Load(); got != 2 {
		t.Fatalf("JWKS fetches after unknown kid = %d, want exactly one refresh (2 total)", got)
	}

	t.Run("a token with a kid that never exists refreshes at most once per cooldown", func(t *testing.T) {
		bogus := p.signRS(t, p.claims(), map[string]any{"kid": "nope"})
		for i := 0; i < 5; i++ {
			if _, err := v.Verify(ctx, bogus); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		}
		// The rotation above already used the one unknown-kid refresh allowed
		// per cooldown, so none of these may reach the provider.
		if got := p.jwksHits.Load(); got != 2 {
			t.Errorf("JWKS fetches = %d, want still 2", got)
		}
	})
}

func TestVerify_JWKSCacheHit(t *testing.T) {
	p := newTestProvider(t)
	v := p.newVerifier()
	for i := 0; i < 10; i++ {
		if _, err := v.Verify(context.Background(), p.signRS(t, p.claims(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if d, j := p.discoveryHits.Load(), p.jwksHits.Load(); d != 1 || j != 1 {
		t.Errorf("discovery hits = %d, jwks hits = %d, want 1 and 1", d, j)
	}
}

func TestVerify_StaleJWKSServedWhenProviderDown(t *testing.T) {
	p := newTestProvider(t)
	v := p.newVerifier()
	now := time.Now()
	v.now = func() time.Time { return now }
	ctx := context.Background()

	if _, err := v.Verify(ctx, p.signRS(t, p.claims(), nil)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour) // past the 1h JWKS TTL
	p.jwksStatus.Store(http.StatusInternalServerError)
	c := p.claims()
	c["exp"] = now.Add(time.Hour).Unix()
	if _, err := v.Verify(ctx, p.signRS(t, c, nil)); err != nil {
		t.Fatalf("expected stale keys to keep working: %v", err)
	}
	if p.jwksHits.Load() != 2 {
		t.Errorf("expected one failed refresh attempt, got %d fetches", p.jwksHits.Load())
	}
}

func TestVerify_ProviderUnavailable(t *testing.T) {
	t.Run("discovery 500", func(t *testing.T) {
		p := newTestProvider(t)
		p.discoveryStatus.Store(500)
		_, err := p.newVerifier().Verify(context.Background(), p.signRS(t, p.claims(), nil))
		if !errors.Is(err, ErrProviderUnavailable) || errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrProviderUnavailable", err)
		}
	})
	t.Run("JWKS 500", func(t *testing.T) {
		p := newTestProvider(t)
		p.jwksStatus.Store(500)
		_, err := p.newVerifier().Verify(context.Background(), p.signRS(t, p.claims(), nil))
		if !errors.Is(err, ErrProviderUnavailable) || errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrProviderUnavailable", err)
		}
	})
	t.Run("provider unreachable", func(t *testing.T) {
		p := newTestProvider(t)
		v := p.newVerifier()
		token := p.signRS(t, p.claims(), nil)
		p.srv.Close()
		if _, err := v.Verify(context.Background(), token); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("err = %v, want ErrProviderUnavailable", err)
		}
	})
	t.Run("malformed token stays a 401 class even while the provider is down", func(t *testing.T) {
		p := newTestProvider(t)
		p.discoveryStatus.Store(500)
		if _, err := p.newVerifier().Verify(context.Background(), "x.y.z"); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})
	t.Run("discovery issuer mismatch", func(t *testing.T) {
		p := newTestProvider(t)
		v := NewVerifier("https://other.example.com", testAudience, nil, time.Hour)
		v.HTTPClient = &http.Client{Transport: rewriteTo(p.srv)}
		if _, err := v.Verify(context.Background(), p.signRS(t, p.claims(), nil)); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("err = %v, want ErrProviderUnavailable", err)
		}
	})
	t.Run("a failed discovery is retried, not cached", func(t *testing.T) {
		p := newTestProvider(t)
		v := p.newVerifier()
		token := p.signRS(t, p.claims(), nil)
		p.discoveryStatus.Store(500)
		if _, err := v.Verify(context.Background(), token); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("err = %v", err)
		}
		p.discoveryStatus.Store(0)
		if _, err := v.Verify(context.Background(), token); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	})
}

// rewriteTo sends every request to srv regardless of the URL's host.
func rewriteTo(srv *httptest.Server) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return http.DefaultTransport.RoundTrip(r2)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVerify_BadJWKSEntriesAreSkipped(t *testing.T) {
	p := newTestProvider(t)
	p.extraKeys = []map[string]any{
		{"kty": "oct", "kid": "sym", "k": b64([]byte("secret"))},
		{"kty": "RSA", "kid": "broken", "n": "!!", "e": "AQAB"},
		{"kty": "RSA", "kid": "enc-only", "use": "enc", "n": b64(p.rsaKey.PublicKey.N.Bytes()), "e": "AQAB"},
	}
	v := p.newVerifier()
	if _, err := v.Verify(context.Background(), p.signRS(t, p.claims(), nil)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, k := range v.jwks.snapshot() {
		if k.kid == "sym" || k.kid == "enc-only" || k.kid == "broken" {
			t.Errorf("key %q should have been skipped", k.kid)
		}
	}
}

func TestCheckStartup(t *testing.T) {
	t.Run("introspection disabled needs nothing", func(t *testing.T) {
		p := newTestProvider(t, withoutIntrospection)
		v := p.newVerifier()
		v.Introspection.Enabled = false
		if err := v.CheckStartup(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("endpoint from discovery", func(t *testing.T) {
		p := newTestProvider(t)
		if err := p.newVerifier().CheckStartup(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no endpoint anywhere fails", func(t *testing.T) {
		p := newTestProvider(t, withoutIntrospection)
		err := p.newVerifier().CheckStartup(context.Background())
		if err == nil || !strings.Contains(err.Error(), "introspection_endpoint") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("explicit endpoint needs no discovery", func(t *testing.T) {
		p := newTestProvider(t, withoutIntrospection)
		v := p.newVerifier()
		v.Introspection.Endpoint = p.srv.URL + "/introspect"
		p.discoveryStatus.Store(500)
		if err := v.CheckStartup(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func newServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
