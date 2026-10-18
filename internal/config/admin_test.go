package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

func TestAdminOIDC_Effective(t *testing.T) {
	base := validOIDC().OIDC
	base.RequiredScopes = []string{"metrics:read"}
	base.Introspection.Enabled = true
	base.Introspection.Endpoint = "https://idp.example.com/introspect"

	t.Run("nothing set: identical to auth.oidc", func(t *testing.T) {
		eff := AdminOIDCConfig{}.Effective(base)
		if !eff.SameVerification(base) {
			t.Errorf("effective %+v differs from base %+v", eff, base)
		}
	})

	t.Run("each field overrides on its own", func(t *testing.T) {
		tests := []struct {
			name  string
			admin AdminOIDCConfig
			check func(OIDCConfig) bool
		}{
			{"audience", AdminOIDCConfig{Audience: "https://admin"}, func(e OIDCConfig) bool { return e.Audience == "https://admin" && e.IssuerURL == base.IssuerURL }},
			{"scopes", AdminOIDCConfig{RequiredScopes: []string{"admin"}}, func(e OIDCConfig) bool { return slices.Equal(e.RequiredScopes, []string{"admin"}) }},
			{"jwks ttl", AdminOIDCConfig{JWKSCacheTTL: time.Hour}, func(e OIDCConfig) bool { return e.JWKSCacheTTL == time.Hour }},
			{"introspection off", AdminOIDCConfig{Introspection: AdminIntrospectionConfig{Enabled: boolPtr(false)}}, func(e OIDCConfig) bool { return !e.Introspection.Enabled && e.Introspection.ClientID == "argvio" }},
			{"introspection ttl", AdminOIDCConfig{Introspection: AdminIntrospectionConfig{CacheTTL: time.Hour}}, func(e OIDCConfig) bool { return e.Introspection.CacheTTL == time.Hour && e.Introspection.Enabled }},
			{"the same issuer keeps credentials", AdminOIDCConfig{IssuerURL: base.IssuerURL}, func(e OIDCConfig) bool { return e.Introspection.ClientSecret == "s" && e.Introspection.Enabled }},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if eff := tt.admin.Effective(base); !tt.check(eff) {
					t.Errorf("effective = %+v", eff)
				}
			})
		}
	})

	t.Run("a different issuer does not inherit introspection credentials", func(t *testing.T) {
		eff := AdminOIDCConfig{IssuerURL: "https://admin-idp.example.com"}.Effective(base)
		in := eff.Introspection
		if in.Enabled || in.Endpoint != "" || in.ClientID != "" || in.ClientSecret != "" {
			t.Errorf("introspection = %+v", in)
		}
		if eff.Audience != base.Audience || !slices.Equal(eff.RequiredScopes, base.RequiredScopes) {
			t.Errorf("audience and scopes should still fall back: %+v", eff)
		}

		own := AdminOIDCConfig{IssuerURL: "https://admin-idp.example.com", Introspection: AdminIntrospectionConfig{
			Enabled: boolPtr(true), ClientID: "admin-client", ClientSecret: "admin-secret",
		}}.Effective(base)
		if !own.Introspection.Enabled || own.Introspection.ClientSecret != "admin-secret" || own.Introspection.CacheTTL != base.Introspection.CacheTTL {
			t.Errorf("own introspection = %+v", own.Introspection)
		}
	})

	t.Run("does not alias the base scopes", func(t *testing.T) {
		eff := AdminOIDCConfig{}.Effective(base)
		eff.RequiredScopes[0] = "changed"
		if base.RequiredScopes[0] != "metrics:read" {
			t.Error("Effective shares its scopes slice with the base config")
		}
	})
}

func TestAuthValidate_Admin(t *testing.T) {
	adminOn := func(a *AuthConfig) { a.Methods = []AuthMethod{AuthMethodOIDC} }
	tests := []struct {
		name        string
		recognition bool
		mutate      func(*AuthConfig)
		wantErr     string // substring; "" = valid
	}{
		{"nothing configured: unchanged", false, func(a *AuthConfig) {}, ""},
		{"nothing configured, no oidc at all", false, func(a *AuthConfig) { a.Methods = []AuthMethod{AuthMethodAPIKey}; a.OIDC = OIDCConfig{} }, ""},
		{"admin oidc settings without a recognition rule", false, func(a *AuthConfig) { a.Admin.OIDC.Audience = "https://admin" }, "no admin recognition mechanism"},
		{"introspection setting without a recognition rule", false, func(a *AuthConfig) {
			a.Admin.OIDC.Introspection.Enabled = boolPtr(false)
		}, "no admin recognition mechanism"},
		{"on, inheriting everything", true, adminOn, ""},
		{"on without the oidc method", true, func(a *AuthConfig) { a.Methods = []AuthMethod{AuthMethodAPIKey} }, "requires auth.methods to include oidc"},
		{"on, its own issuer and audience", true, func(a *AuthConfig) {
			adminOn(a)
			a.Admin.OIDC.IssuerURL, a.Admin.OIDC.Audience = "https://admin-idp.example.com", "https://admin"
		}, ""},
		{"on, issuer missing everywhere", true, func(a *AuthConfig) { adminOn(a); a.OIDC.IssuerURL = "" }, "auth.admin.oidc.issuer_url"},
		{"on, audience missing everywhere", true, func(a *AuthConfig) { adminOn(a); a.OIDC.Audience = "" }, "auth.admin.oidc.audience"},
		{"on, issuer must be an absolute URL", true, func(a *AuthConfig) { adminOn(a); a.Admin.OIDC.IssuerURL = "admin-idp" }, "auth.admin.oidc.issuer_url"},
		{"on, jwks ttl falls back", true, func(a *AuthConfig) { adminOn(a); a.Admin.OIDC.JWKSCacheTTL = 0 }, ""},
		{"on, introspection inherited and valid", true, func(a *AuthConfig) { adminOn(a); a.OIDC.Introspection.Enabled = true }, ""},
		{"on, different issuer turning introspection on needs its own credentials", true, func(a *AuthConfig) {
			adminOn(a)
			a.OIDC.Introspection.Enabled = true
			a.Admin.OIDC.IssuerURL = "https://admin-idp.example.com"
			a.Admin.OIDC.Introspection.Enabled = boolPtr(true)
		}, "auth.admin.oidc.introspection.client_id"},
		{"on, different issuer inherits introspection off", true, func(a *AuthConfig) {
			adminOn(a)
			a.OIDC.Introspection.Enabled = true
			a.Admin.OIDC.IssuerURL = "https://admin-idp.example.com"
		}, ""},
		{"on, introspection endpoint must be a URL", true, func(a *AuthConfig) {
			adminOn(a)
			a.Admin.OIDC.Introspection = AdminIntrospectionConfig{Enabled: boolPtr(true), Endpoint: "nope"}
		}, "auth.admin.oidc.introspection.endpoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validOIDC()
			tt.mutate(&a)
			errs := a.validateAdmin(tt.recognition)
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

// TestAdminDisabledForNow documents that, until a recognition rule exists,
// auth.admin cannot be switched on, so a deployment behaves as it did before.
func TestAdminDisabledForNow(t *testing.T) {
	if (AdminConfig{}).Enabled() {
		t.Fatal("admin must be off with nothing configured")
	}
	a := validOIDC()
	a.Admin.OIDC.Audience = "https://admin"
	if errs := a.validate(); len(errs) == 0 {
		t.Error("an admin setting with nothing to recognise admins must fail startup validation")
	}
}

func TestLoad_AdminKeys(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")

	t.Run("unset: every field inherits", func(t *testing.T) {
		c, err := Load(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Auth.Admin.OIDC.isZero() {
			t.Errorf("admin oidc = %+v, want all unset", c.Auth.Admin.OIDC)
		}
		if c.Auth.Admin.OIDC.Introspection.Enabled != nil {
			t.Error("introspection.enabled must default to nil (inherit), not false")
		}
	})

	t.Run("from env", func(t *testing.T) {
		t.Setenv("ARGVIO_AUTH__ADMIN__OIDC__ISSUER_URL", "https://admin-idp.example.com")
		t.Setenv("ARGVIO_AUTH__ADMIN__OIDC__AUDIENCE", "https://admin")
		t.Setenv("ARGVIO_AUTH__ADMIN__OIDC__REQUIRED_SCOPES", "a,b")
		t.Setenv("ARGVIO_AUTH__ADMIN__OIDC__JWKS_CACHE_TTL", "5m")
		t.Setenv("ARGVIO_AUTH__ADMIN__OIDC__INTROSPECTION__ENABLED", "false")
		t.Setenv("ARGVIO_AUTH__ADMIN__OIDC__INTROSPECTION__CACHE_TTL", "30s")
		c, err := Load(nil)
		if err != nil {
			t.Fatal(err)
		}
		o := c.Auth.Admin.OIDC
		if o.IssuerURL != "https://admin-idp.example.com" || o.Audience != "https://admin" || !slices.Equal(o.RequiredScopes, []string{"a", "b"}) || o.JWKSCacheTTL != 5*time.Minute {
			t.Errorf("admin oidc = %+v", o)
		}
		if o.Introspection.Enabled == nil || *o.Introspection.Enabled || o.Introspection.CacheTTL != 30*time.Second {
			t.Errorf("introspection = %+v (enabled must be an explicit false)", o.Introspection)
		}
	})
}
