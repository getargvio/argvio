package config

import (
	"strings"
	"testing"
	"time"
)

func validOIDC() AuthConfig {
	return AuthConfig{
		Methods: []AuthMethod{AuthMethodOIDC},
		OIDC: OIDCConfig{
			IssuerURL:    "https://idp.example.com",
			Audience:     "https://api.example.com",
			JWKSCacheTTL: time.Minute,
			Introspection: OIDCIntrospectionConfig{
				ClientID: "argvio", ClientSecret: "s", CacheTTL: time.Minute, NegativeCacheTTL: time.Second,
			},
		},
	}
}

func TestAuthValidate_OIDC(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AuthConfig)
		wantErr string // substring; "" = valid
	}{
		{"valid", func(a *AuthConfig) {}, ""},
		{"issuer with trailing slash is valid", func(a *AuthConfig) { a.OIDC.IssuerURL = "https://idp.example.com/" }, ""},
		{"issuer must be absolute", func(a *AuthConfig) { a.OIDC.IssuerURL = "idp.example.com" }, "auth.oidc.issuer_url"},
		{"issuer must not carry a query", func(a *AuthConfig) { a.OIDC.IssuerURL = "https://idp.example.com/?x=1" }, "auth.oidc.issuer_url"},
		{"introspection disabled ignores its fields", func(a *AuthConfig) { a.OIDC.Introspection = OIDCIntrospectionConfig{} }, ""},
		{"introspection needs oidc method", func(a *AuthConfig) {
			a.Methods = []AuthMethod{AuthMethodAPIKey}
			a.OIDC.Introspection.Enabled = true
		}, "requires auth.methods to include oidc"},
		{"introspection enabled, endpoint left to discovery", func(a *AuthConfig) { a.OIDC.Introspection.Enabled = true }, ""},
		{"introspection needs client_id", func(a *AuthConfig) {
			a.OIDC.Introspection.Enabled, a.OIDC.Introspection.ClientID = true, ""
		}, "introspection.client_id"},
		{"introspection needs client_secret", func(a *AuthConfig) {
			a.OIDC.Introspection.Enabled, a.OIDC.Introspection.ClientSecret = true, ""
		}, "introspection.client_secret"},
		{"introspection cache_ttl must be positive", func(a *AuthConfig) {
			a.OIDC.Introspection.Enabled, a.OIDC.Introspection.CacheTTL = true, 0
		}, "cache_ttl"},
		{"introspection negative_cache_ttl must be positive", func(a *AuthConfig) {
			a.OIDC.Introspection.Enabled, a.OIDC.Introspection.NegativeCacheTTL = true, 0
		}, "negative_cache_ttl"},
		{"introspection endpoint must be a URL", func(a *AuthConfig) {
			a.OIDC.Introspection.Enabled, a.OIDC.Introspection.Endpoint = true, "not a url"
		}, "introspection.endpoint"},
		{"introspection endpoint override is valid", func(a *AuthConfig) {
			a.OIDC.Introspection.Enabled, a.OIDC.Introspection.Endpoint = true, "https://idp.example.com/oauth/introspect"
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validOIDC()
			tt.mutate(&a)
			errs := a.validate()
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if !strings.Contains(strings.Join(errs, "\n"), tt.wantErr) {
				t.Fatalf("errors %v do not mention %q", errs, tt.wantErr)
			}
		})
	}
}

func TestCORSValidate(t *testing.T) {
	tests := []struct {
		name    string
		origins []string
		wantErr string
	}{
		{"disabled", nil, ""},
		{"https origin", []string{"https://dashboard.example.com"}, ""},
		{"origin with port", []string{"http://localhost:5173"}, ""},
		{"several", []string{"https://a.example.com", "https://b.example.com"}, ""},
		{"wildcard", []string{"*"}, "wildcards are not supported"},
		{"wildcard host", []string{"https://*.example.com"}, "wildcards are not supported"},
		{"missing scheme", []string{"dashboard.example.com"}, "must be an origin"},
		{"non-http scheme", []string{"ftp://dashboard.example.com"}, "must be an origin"},
		{"trailing slash", []string{"https://dashboard.example.com/"}, "no path"},
		{"path", []string{"https://dashboard.example.com/app"}, "no path"},
		{"userinfo", []string{"https://u:p@dashboard.example.com"}, "must be an origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := CORSConfig{AllowedOrigins: tt.origins, MaxAge: time.Minute}.validate()
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if !strings.Contains(strings.Join(errs, "\n"), tt.wantErr) {
				t.Fatalf("errors %v do not mention %q", errs, tt.wantErr)
			}
		})
	}
}

func TestLoad_OIDCDefaultsAndEnvLists(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	t.Setenv("ARGVIO_AUTH__METHODS", "api_key,oidc")
	t.Setenv("ARGVIO_METRICS__CORS__ALLOWED_ORIGINS", "https://a.example.com,https://b.example.com")
	t.Setenv("ARGVIO_AUTH__OIDC__INTROSPECTION__CLIENT_SECRET", "from-env")

	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Auth.Methods) != 2 || !c.Auth.Has(AuthMethodOIDC) {
		t.Errorf("methods = %v", c.Auth.Methods)
	}
	if got := c.Metrics.CORS.AllowedOrigins; len(got) != 2 || got[1] != "https://b.example.com" {
		t.Errorf("allowed_origins = %v", got)
	}
	in := c.Auth.OIDC.Introspection
	if in.Enabled || in.CacheTTL != time.Minute || in.NegativeCacheTTL != 5*time.Second || in.ClientSecret != "from-env" {
		t.Errorf("introspection = %+v", in)
	}
	if c.Auth.OIDC.RequireAccessTokenTyp || c.Auth.OIDC.TrustTenantIDClaim {
		t.Error("require_access_token_typ and trust_tenant_id_claim must default to false")
	}
	if len(c.Metrics.CORS.AllowedOrigins) == 0 {
		t.Error("expected origins")
	}
}

func TestLoad_CORSDisabledByDefault(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Metrics.CORS.AllowedOrigins) != 0 {
		t.Errorf("allowed_origins = %v, want empty (disabled)", c.Metrics.CORS.AllowedOrigins)
	}
}
