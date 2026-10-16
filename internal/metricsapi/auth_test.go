package metricsapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/oidc/oidctest"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
	"github.com/getargvio/argvio/internal/tenant/tenanttest"
)

const testAPIKey = "argv_live_qry_abcdef"

type authFixture struct {
	idp  *oidctest.Provider
	res  *tenanttest.Resolver
	logs *strings.Builder
}

func newAuthFixture(t *testing.T) *authFixture {
	return &authFixture{idp: oidctest.New(t), res: tenanttest.New(), logs: &strings.Builder{}}
}

func (f *authFixture) authConfig(introspect bool, mutate func(*config.AuthConfig)) config.AuthConfig {
	c := config.AuthConfig{
		Methods: []config.AuthMethod{config.AuthMethodAPIKey, config.AuthMethodOIDC},
		OIDC: config.OIDCConfig{
			IssuerURL: f.idp.Issuer(), Audience: oidctest.Audience, JWKSCacheTTL: time.Hour,
			RequiredScopes: []string{"metrics:read"},
			Introspection: config.OIDCIntrospectionConfig{
				Enabled: introspect, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
				CacheTTL: time.Minute, NegativeCacheTTL: time.Second,
			},
		},
	}
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func (f *authFixture) server(cfg config.AuthConfig, cors config.CORSConfig) *Server {
	return &Server{
		Verifier: authn.New(cfg, f.res, tenant.ScopeMetricsQuery),
		CORS:     cors,
		Log:      slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

type probeResult struct {
	called bool
	tenant uuid.UUID
	role   string
}

// probe is a handler that records the tenant scope authMiddleware injected.
func probe(res *probeResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res.called = true
		scope, hasScope := scopeFromContext(r.Context())
		if id, ok := identityFromContext(r.Context()); ok {
			res.role = id.Role
			// The scope must be exactly the one the resolved identity yields.
			if want, err := storage.NewTenantScope(id.Resolved.Tenant.ID); err == nil && hasScope && scope == want {
				res.tenant = id.Resolved.Tenant.ID
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

func do(h http.Handler, method, path, token string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type errBody struct {
	Error string `json:"error"`
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body %q is not an error JSON: %v", rec.Body.String(), err)
	}
	return b.Error
}

func TestAuthMiddleware_OIDCJWT(t *testing.T) {
	f := newAuthFixture(t)
	acme := f.res.AddTenant("acme", "active")
	f.res.AddMember(f.idp.Issuer(), "alice", acme, "admin")
	srv := f.server(f.authConfig(false, nil), config.CORSConfig{})

	t.Run("valid token with the required scope", func(t *testing.T) {
		var res probeResult
		rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", f.idp.Sign(t, f.idp.Claims("alice")), nil)
		if rec.Code != 200 || res.tenant != acme || res.role != "admin" {
			t.Fatalf("status=%d tenant=%v role=%q body=%s", rec.Code, res.tenant, res.role, rec.Body)
		}
	})

	t.Run("missing scope is 403 insufficient_scope, not 401", func(t *testing.T) {
		c := f.idp.Claims("alice")
		c["scope"] = "openid"
		var res probeResult
		rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", f.idp.Sign(t, c), nil)
		if rec.Code != 403 || errCode(t, rec) != "insufficient_scope" || res.called {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="insufficient_scope", scope="metrics:read"` {
			t.Errorf("WWW-Authenticate = %q", got)
		}
	})

	t.Run("no token is 401 with a bare Bearer challenge", func(t *testing.T) {
		var res probeResult
		rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", "", nil)
		if rec.Code != 401 || res.called {
			t.Fatalf("status=%d", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("WWW-Authenticate = %q", got)
		}
	})

	bad := map[string]func() string{
		"token from a different issuer": func() string { return oidctest.New(t).Sign(t, f.idp.Claims("alice")) },
		"expired token": func() string {
			c := f.idp.Claims("alice")
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			return f.idp.Sign(t, c)
		},
		"ID token (audience is the SPA's client_id)": func() string {
			c := f.idp.Claims("alice")
			c["aud"] = "spa-client-id"
			c["nonce"] = "abc"
			return f.idp.Sign(t, c)
		},
		"garbage": func() string { return "not.a.jwt" },
	}
	for name, mk := range bad {
		t.Run(name+" is 401 invalid_token", func(t *testing.T) {
			var res probeResult
			rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", mk(), nil)
			if rec.Code != 401 || res.called {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
		})
	}

	t.Run("IdP outage is 503, never 401", func(t *testing.T) {
		idp := oidctest.New(t)
		idp.JWKSStatus.Store(500)
		f2 := &authFixture{idp: idp, res: f.res, logs: f.logs}
		srv := f2.server(f2.authConfig(false, nil), config.CORSConfig{})
		var res probeResult
		rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", idp.Sign(t, idp.Claims("alice")), nil)
		if rec.Code != 503 || res.called {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Error("a 503 must not carry a Bearer challenge: it would tell the client to re-authenticate")
		}
	})

	t.Run("suspended tenant is 403", func(t *testing.T) {
		gone := f.res.AddTenant("gone", "suspended")
		f.res.AddMember(f.idp.Issuer(), "carol", gone, "admin")
		rec := do(srv.authMiddleware(probe(&probeResult{})), "GET", "/v1/traces", f.idp.Sign(t, f.idp.Claims("carol")), nil)
		if rec.Code != 403 {
			t.Fatalf("status=%d", rec.Code)
		}
	})

	t.Run("the token is never logged", func(t *testing.T) {
		token := f.idp.Sign(t, f.idp.Claims("alice"))
		do(srv.authMiddleware(probe(&probeResult{})), "GET", "/v1/traces", token, nil)
		do(srv.authMiddleware(probe(&probeResult{})), "GET", "/v1/traces", token+"x", nil)
		logged := f.logs.String()
		if strings.Contains(logged, token) || strings.Contains(logged, strings.Split(token, ".")[1]) {
			t.Error("token (or its payload) found in logs")
		}
		if !strings.Contains(logged, "iss="+f.idp.Issuer()) || !strings.Contains(logged, "sub=alice") {
			t.Errorf("expected iss/sub in the debug log, got:\n%s", logged)
		}
	})
}

func TestAuthMiddleware_TenantSelectionMatrix(t *testing.T) {
	f := newAuthFixture(t)
	a := f.res.AddTenant("a", "active")
	b := f.res.AddTenant("b", "active")
	foreign := f.res.AddTenant("foreign", "active")
	f.res.AddMember(f.idp.Issuer(), "one", a, "viewer")
	f.res.AddMember(f.idp.Issuer(), "many", a, "viewer")
	f.res.AddMember(f.idp.Issuer(), "many", b, "admin")
	srv := f.server(f.authConfig(false, nil), config.CORSConfig{})

	tests := []struct {
		name       string
		sub        string
		header     string
		wantStatus int
		wantCode   string
		wantTenant uuid.UUID
	}{
		{"0 memberships, no header", "none", "", 403, "no_tenant_access", uuid.Nil},
		{"0 memberships, foreign header", "none", foreign.String(), 403, "no_tenant_access", uuid.Nil},
		{"1 membership, no header", "one", "", 200, "", a},
		{"1 membership, matching header", "one", a.String(), 200, "", a},
		{"1 membership, foreign header", "one", foreign.String(), 403, "no_tenant_access", uuid.Nil},
		{"many memberships, no header", "many", "", 400, "tenant_required", uuid.Nil},
		{"many memberships, valid header", "many", b.String(), 200, "", b},
		{"many memberships, foreign header", "many", foreign.String(), 403, "no_tenant_access", uuid.Nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var res probeResult
			hdr := map[string]string{}
			if tt.header != "" {
				hdr[TenantHeader] = tt.header
			}
			rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", f.idp.Sign(t, f.idp.Claims(tt.sub)), hdr)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" && errCode(t, rec) != tt.wantCode {
				t.Errorf("error code = %q, want %q", errCode(t, rec), tt.wantCode)
			}
			if res.tenant != tt.wantTenant {
				t.Errorf("scoped tenant = %v, want %v", res.tenant, tt.wantTenant)
			}
			if tt.wantStatus == 403 && rec.Header().Get("WWW-Authenticate") != "" {
				t.Error("a tenant 403 must not carry a Bearer challenge: the client must not re-login")
			}
		})
	}
}

func TestAuthMiddleware_OpaqueToken(t *testing.T) {
	f := newAuthFixture(t)
	acme := f.res.AddTenant("acme", "active")
	f.res.AddMember(f.idp.Issuer(), "alice", acme, "admin")
	srv := f.server(f.authConfig(true, nil), config.CORSConfig{})

	f.idp.AddOpaque("active-token", "alice", nil)
	f.idp.AddOpaque("wrong-aud", "alice", func(m map[string]any) { m["aud"] = "https://elsewhere" })
	f.idp.AddOpaque("expired", "alice", func(m map[string]any) { m["exp"] = time.Now().Add(-time.Hour).Unix() })
	f.idp.AddOpaque("inactive", "alice", func(m map[string]any) { m["active"] = false })
	f.idp.AddOpaque("no-scope", "alice", func(m map[string]any) { m["scope"] = "openid" })

	tests := []struct {
		token      string
		wantStatus int
		wantCode   string
	}{
		{"active-token", 200, ""},
		{"wrong-aud", 401, "unauthorized"},
		{"expired", 401, "unauthorized"},
		{"inactive", 401, "unauthorized"},
		{"never-issued", 401, "unauthorized"},
		{"no-scope", 403, "insufficient_scope"},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			var res probeResult
			rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", tt.token, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" && errCode(t, rec) != tt.wantCode {
				t.Errorf("code = %q", errCode(t, rec))
			}
			if tt.wantStatus == 200 && res.tenant != acme {
				t.Errorf("tenant = %v", res.tenant)
			}
		})
	}

	t.Run("a cache hit does not call the IdP again", func(t *testing.T) {
		before := f.idp.IntrospectHits.Load()
		for i := 0; i < 5; i++ {
			if rec := do(srv.authMiddleware(probe(&probeResult{})), "GET", "/v1/traces", "active-token", nil); rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
		}
		if got := f.idp.IntrospectHits.Load() - before; got != 0 {
			t.Errorf("IdP calls = %d, want 0", got)
		}
	})

	t.Run("IdP 500 is 503", func(t *testing.T) {
		f.idp.AddOpaque("fresh-token", "alice", nil)
		f.idp.IntrospectStatus.Store(500)
		defer f.idp.IntrospectStatus.Store(0)
		rec := do(srv.authMiddleware(probe(&probeResult{})), "GET", "/v1/traces", "fresh-token", nil)
		if rec.Code != 503 {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	})
}

func TestAuthMiddleware_APIKeyCallersUnaffected(t *testing.T) {
	f := newAuthFixture(t)
	acme := f.res.AddTenant("acme", "active")
	other := f.res.AddTenant("other", "active")
	f.res.AddAPIKey(testAPIKey, acme)

	for name, introspect := range map[string]bool{"introspection off": false, "introspection on": true} {
		t.Run(name, func(t *testing.T) {
			srv := f.server(f.authConfig(introspect, nil), config.CORSConfig{})
			var res probeResult
			rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", testAPIKey, nil)
			if rec.Code != 200 || res.tenant != acme {
				t.Fatalf("status=%d tenant=%v", rec.Code, res.tenant)
			}
			rec = do(srv.authMiddleware(probe(&probeResult{})), "GET", "/v1/traces", testAPIKey, map[string]string{TenantHeader: other.String()})
			if rec.Code != 403 || errCode(t, rec) != "no_tenant_access" {
				t.Fatalf("foreign tenant header: status=%d body=%s", rec.Code, rec.Body)
			}
			rec = do(srv.authMiddleware(probe(&probeResult{})), "GET", "/v1/traces", "argv_live_qry_unknown", nil)
			if rec.Code != 401 {
				t.Fatalf("unknown key: status=%d", rec.Code)
			}
		})
	}
	if n := f.idp.IntrospectHits.Load() + f.idp.JWKSHits.Load(); n != 0 {
		t.Errorf("API keys caused %d IdP requests", n)
	}
}

func TestTenantsEndpoint(t *testing.T) {
	f := newAuthFixture(t)
	a := f.res.AddTenant("Acme", "active")
	b := f.res.AddTenant("Beta", "active")
	f.res.AddMember(f.idp.Issuer(), "many", a, "viewer")
	f.res.AddMember(f.idp.Issuer(), "many", b, "admin")
	f.res.AddAPIKey(testAPIKey, a)
	h := f.server(f.authConfig(true, nil), config.CORSConfig{}).Handler()

	t.Run("lists memberships without needing a tenant header", func(t *testing.T) {
		rec := do(h, "GET", "/v1/tenants", f.idp.Sign(t, f.idp.Claims("many")), nil)
		if rec.Code != 200 {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
		}
		var got []map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body must be a bare array: %v (%s)", err, rec.Body)
		}
		if len(got) != 2 || got[0]["id"] != a.String() || got[0]["name"] != "Acme" || got[0]["role"] != "viewer" || got[1]["role"] != "admin" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("works with an opaque token", func(t *testing.T) {
		f.idp.AddOpaque("opaque-many", "many", nil)
		if rec := do(h, "GET", "/v1/tenants", "opaque-many", nil); rec.Code != 200 {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
		}
	})
	t.Run("no memberships is an empty array, not an error", func(t *testing.T) {
		rec := do(h, "GET", "/v1/tenants", f.idp.Sign(t, f.idp.Claims("nobody")), nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("status=%d body=%q", rec.Code, rec.Body)
		}
	})
	t.Run("ignores the tenant header", func(t *testing.T) {
		rec := do(h, "GET", "/v1/tenants", f.idp.Sign(t, f.idp.Claims("many")), map[string]string{TenantHeader: uuid.NewString()})
		if rec.Code != 200 {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("401 without a token, 403 insufficient_scope without the scope", func(t *testing.T) {
		if rec := do(h, "GET", "/v1/tenants", "", nil); rec.Code != 401 {
			t.Errorf("no token: status = %d", rec.Code)
		}
		c := f.idp.Claims("many")
		c["scope"] = "openid"
		if rec := do(h, "GET", "/v1/tenants", f.idp.Sign(t, c), nil); rec.Code != 403 {
			t.Errorf("no scope: status = %d", rec.Code)
		}
	})
	t.Run("an API key is not an OIDC identity", func(t *testing.T) {
		if rec := do(h, "GET", "/v1/tenants", testAPIKey, nil); rec.Code != 403 {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})
}

func TestCORS(t *testing.T) {
	const uiOrigin = "https://dashboard.example.com"
	f := newAuthFixture(t)
	f.res.AddTenant("acme", "active")
	cfg := f.authConfig(false, nil)
	enabled := f.server(cfg, config.CORSConfig{AllowedOrigins: []string{uiOrigin}, MaxAge: 10 * time.Minute}).Handler()
	disabled := f.server(cfg, config.CORSConfig{}).Handler()

	preflight := func(h http.Handler, origin string) *httptest.ResponseRecorder {
		return do(h, "OPTIONS", "/v1/tenants", "", map[string]string{
			"Origin":                         origin,
			"Access-Control-Request-Method":  "GET",
			"Access-Control-Request-Headers": "authorization,content-type,x-argvio-tenant",
		})
	}

	t.Run("preflight from the UI origin needs no token", func(t *testing.T) {
		rec := preflight(enabled, uiOrigin)
		if rec.Code != 204 {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		h := rec.Header()
		if h.Get("Access-Control-Allow-Origin") != uiOrigin {
			t.Errorf("Allow-Origin = %q", h.Get("Access-Control-Allow-Origin"))
		}
		for _, want := range []string{"Authorization", "X-Argvio-Tenant", "Content-Type"} {
			if !strings.Contains(strings.ToLower(h.Get("Access-Control-Allow-Headers")), strings.ToLower(want)) {
				t.Errorf("Allow-Headers %q lacks %s", h.Get("Access-Control-Allow-Headers"), want)
			}
		}
		if m := h.Get("Access-Control-Allow-Methods"); !strings.Contains(m, "GET") || strings.Contains(m, "POST") {
			t.Errorf("Allow-Methods = %q", m)
		}
		if h.Get("Access-Control-Max-Age") != "600" {
			t.Errorf("Max-Age = %q", h.Get("Access-Control-Max-Age"))
		}
		if h.Get("Access-Control-Allow-Credentials") != "" {
			t.Error("credentials must not be allowed")
		}
	})

	t.Run("no CORS headers for another origin", func(t *testing.T) {
		for _, origin := range []string{"https://evil.example.com", "http://dashboard.example.com", "https://dashboard.example.com:8443", "https://sub.dashboard.example.com"} {
			rec := preflight(enabled, origin)
			for k := range rec.Header() {
				if strings.HasPrefix(k, "Access-Control-") && k != "Access-Control-Request-Method" {
					t.Errorf("origin %s: unexpected header %s: %s", origin, k, rec.Header().Get(k))
				}
			}
			if rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Errorf("origin %s was allowed", origin)
			}
		}
	})

	t.Run("an actual response exposes WWW-Authenticate to the UI", func(t *testing.T) {
		rec := do(enabled, "GET", "/v1/tenants", "", map[string]string{"Origin": uiOrigin})
		if rec.Code != 401 {
			t.Fatalf("status = %d", rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != uiOrigin {
			t.Error("401 response lacks Allow-Origin: the SPA could not read it")
		}
		// Header names are case-insensitive; the CORS library canonicalises them.
		if !strings.Contains(strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers")), "www-authenticate") {
			t.Errorf("Expose-Headers = %q", rec.Header().Get("Access-Control-Expose-Headers"))
		}
	})

	t.Run("a disallowed origin gets no CORS headers on a real request either", func(t *testing.T) {
		rec := do(enabled, "GET", "/v1/tenants", "", map[string]string{"Origin": "https://evil.example.com"})
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("Allow-Origin set for a disallowed origin")
		}
	})

	t.Run("disabled by default", func(t *testing.T) {
		rec := preflight(disabled, uiOrigin)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("CORS answered while disabled")
		}
	})
}

func TestLegacyJWTStillWorks(t *testing.T) {
	f := newAuthFixture(t)
	acme := f.res.AddTenant("acme", "active")
	cfg := f.authConfig(false, func(c *config.AuthConfig) {
		c.Methods = []config.AuthMethod{config.AuthMethodJWT}
		c.JWTSigningKey = "secret"
	})
	srv := f.server(cfg, config.CORSConfig{})
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"tenant_id": acme.String(), "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("secret"))

	var res probeResult
	if rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", tok, nil); rec.Code != 200 || res.tenant != acme {
		t.Fatalf("status=%d tenant=%v", rec.Code, res.tenant)
	}
}
