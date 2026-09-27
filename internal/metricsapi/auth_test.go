package metricsapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/tenant"
)

// fakeResolver is a minimal in-memory tenant.Resolver, keyed only by tenant
// ID, sufficient for exercising authn.Verifier's jwt/oidc path without a
// real Postgres-backed tenant.Cache.
type fakeResolver struct {
	byTenantID map[uuid.UUID]*tenant.Resolved
}

func (f *fakeResolver) Resolve(ctx context.Context, rawKey, scope string) (*tenant.Resolved, bool, error) {
	return nil, false, nil
}

func (f *fakeResolver) ResolveByTenantID(ctx context.Context, tenantID uuid.UUID) (*tenant.Resolved, bool, error) {
	r, ok := f.byTenantID[tenantID]
	return r, ok, nil
}

// fakeOIDCProvider serves a minimal discovery document + JWKS backed by a
// freshly generated RSA key, for exercising the oidc auth method without a
// real identity provider.
type fakeOIDCProvider struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &fakeOIDCProvider{key: key, kid: "k1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":   p.srv.URL,
			"jwks_uri": p.srv.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		e := p.key.PublicKey.E
		eb := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": p.kid,
				"use": "sig",
				"n":   base64.RawURLEncoding.EncodeToString(p.key.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(eb),
			}},
		})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeOIDCProvider) sign(t *testing.T, tenantID uuid.UUID, scope string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":       p.srv.URL,
		"aud":       "argvio-metrics",
		"tenant_id": tenantID.String(),
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
	if scope != "" {
		claims["scope"] = scope
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = p.kid
	s, err := tok.SignedString(p.key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func newOIDCTestServer(p *fakeOIDCProvider, requiredScopes []string, tenantID uuid.UUID) *Server {
	resolver := &fakeResolver{byTenantID: map[uuid.UUID]*tenant.Resolved{
		tenantID: {Tenant: tenant.Tenant{ID: tenantID, Status: "active"}},
	}}
	authCfg := config.AuthConfig{
		Methods: []config.AuthMethod{config.AuthMethodOIDC},
		OIDC: config.OIDCConfig{
			IssuerURL:      p.srv.URL,
			Audience:       "argvio-metrics",
			RequiredScopes: requiredScopes,
			JWKSCacheTTL:   time.Minute,
		},
	}
	return &Server{Verifier: authn.New(authCfg, resolver, tenant.ScopeMetricsQuery)}
}

func TestAuthMiddleware_OIDC(t *testing.T) {
	p := newFakeOIDCProvider(t)
	tenantID := uuid.New()
	srv := newOIDCTestServer(p, []string{"metrics:read"}, tenantID)

	var sawScope bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFromContext(r.Context())
		sawScope = ok
		_ = scope
		w.WriteHeader(http.StatusOK)
	})

	t.Run("valid token with required scope is accepted", func(t *testing.T) {
		sawScope = false
		req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
		req.Header.Set("Authorization", "Bearer "+p.sign(t, tenantID, "metrics:read"))
		rec := httptest.NewRecorder()
		srv.authMiddleware(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
		if !sawScope {
			t.Error("expected tenant scope to be injected into request context")
		}
	})

	t.Run("missing required scope is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
		req.Header.Set("Authorization", "Bearer "+p.sign(t, tenantID, "openid"))
		rec := httptest.NewRecorder()
		srv.authMiddleware(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("no token is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
		rec := httptest.NewRecorder()
		srv.authMiddleware(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("token from a different issuer is rejected", func(t *testing.T) {
		other := newFakeOIDCProvider(t)
		req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
		req.Header.Set("Authorization", "Bearer "+other.sign(t, tenantID, "metrics:read"))
		rec := httptest.NewRecorder()
		srv.authMiddleware(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("token missing tenant_id claim is rejected", func(t *testing.T) {
		claims := jwt.MapClaims{
			"iss":   p.srv.URL,
			"aud":   "argvio-metrics",
			"scope": "metrics:read",
			"exp":   time.Now().Add(time.Hour).Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = p.kid
		signed, err := tok.SignedString(p.key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
		req.Header.Set("Authorization", "Bearer "+signed)
		rec := httptest.NewRecorder()
		srv.authMiddleware(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("suspended tenant is rejected with 403", func(t *testing.T) {
		suspendedID := uuid.New()
		suspendedSrv := newOIDCTestServer(p, nil, suspendedID)
		suspendedSrv.Verifier = authn.New(config.AuthConfig{
			Methods: []config.AuthMethod{config.AuthMethodOIDC},
			OIDC: config.OIDCConfig{
				IssuerURL:    p.srv.URL,
				Audience:     "argvio-metrics",
				JWKSCacheTTL: time.Minute,
			},
		}, &fakeResolver{byTenantID: map[uuid.UUID]*tenant.Resolved{
			suspendedID: {Tenant: tenant.Tenant{ID: suspendedID, Status: "suspended"}},
		}}, tenant.ScopeMetricsQuery)

		req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
		req.Header.Set("Authorization", "Bearer "+p.sign(t, suspendedID, ""))
		rec := httptest.NewRecorder()
		suspendedSrv.authMiddleware(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}
