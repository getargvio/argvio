package config

import (
	"net/url"
	"time"
)

type AuthMethod string

const (
	AuthMethodAPIKey AuthMethod = "api_key" // scoped tenant API key
	AuthMethodJWT    AuthMethod = "jwt"     // dashboard-user session/JWT (HS256)
	AuthMethodOIDC   AuthMethod = "oidc"    // third-party OIDC provider (Auth0, Okta, Azure AD, ...)
)

// OIDCIntrospectionConfig configures RFC 7662 token introspection, which is
// how opaque (non-JWT) access tokens from the OIDC provider are verified.
type OIDCIntrospectionConfig struct {
	Enabled bool `koanf:"enabled"`
	// Endpoint defaults to the provider's discovery `introspection_endpoint`.
	Endpoint     string `koanf:"endpoint"`
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`
	// CacheTTL bounds how long an active result is reused, so it is also the
	// worst-case latency before a revoked token stops being accepted.
	CacheTTL         time.Duration `koanf:"cache_ttl"`
	NegativeCacheTTL time.Duration `koanf:"negative_cache_ttl"`
}

// OIDCConfig configures third-party OIDC token verification.
type OIDCConfig struct {
	IssuerURL      string        `koanf:"issuer_url"`
	Audience       string        `koanf:"audience"`
	RequiredScopes []string      `koanf:"required_scopes"`
	JWKSCacheTTL   time.Duration `koanf:"jwks_cache_ttl"`

	// RequireAccessTokenTyp rejects a JWT whose header carries a `typ` other
	// than `at+jwt` (RFC 9068), which is how an ID token is told apart from an
	// access token when the provider sets `typ`.
	RequireAccessTokenTyp bool `koanf:"require_access_token_typ"`

	// TrustTenantIDClaim lets a `tenant_id` claim select the tenant without a
	// tenant_members row. Off by default: a claim must be a membership.
	TrustTenantIDClaim bool `koanf:"trust_tenant_id_claim"`

	Introspection OIDCIntrospectionConfig `koanf:"introspection"`
}

// AuthConfig is the global auth config shared by every server.
type AuthConfig struct {
	Methods []AuthMethod `koanf:"methods"`

	JWTSigningKey string     `koanf:"jwt_signing_key"`
	OIDC          OIDCConfig `koanf:"oidc"`

	// Admin is honoured by the metrics server only.
	Admin AdminConfig `koanf:"admin"`
}

func (a AuthConfig) Has(m AuthMethod) bool {
	for _, cfg := range a.Methods {
		if cfg == m {
			return true
		}
	}
	return false
}

func authDefaults() map[string]any {
	return map[string]any{
		"auth.methods": []string{"api_key", "jwt"},

		"auth.jwt_signing_key": "",

		"auth.oidc.issuer_url":      "",
		"auth.oidc.audience":        "",
		"auth.oidc.required_scopes": []string{},
		"auth.oidc.jwks_cache_ttl":  "15m",

		"auth.oidc.require_access_token_typ": false,
		"auth.oidc.trust_tenant_id_claim":    false,

		"auth.oidc.introspection.enabled":            false,
		"auth.oidc.introspection.endpoint":           "",
		"auth.oidc.introspection.client_id":          "",
		"auth.oidc.introspection.client_secret":      "",
		"auth.oidc.introspection.cache_ttl":          "60s",
		"auth.oidc.introspection.negative_cache_ttl": "5s",

		"auth.admin.claim.name":  "",
		"auth.admin.claim.value": "",
		"auth.admin.subjects":    []string{},
		"auth.admin.role":        "viewer",
		"auth.admin.role_claim":  "",
	}
}

func (a AuthConfig) validate() []string {
	var errs []string
	if len(a.Methods) == 0 {
		errs = append(errs, "auth.methods must not be empty")
	}
	for _, m := range a.Methods {
		switch m {
		case AuthMethodAPIKey:
		case AuthMethodJWT:
			if a.JWTSigningKey == "" {
				errs = append(errs, "auth.jwt_signing_key must be set when auth.methods includes jwt")
			}
		case AuthMethodOIDC:
			if a.OIDC.IssuerURL == "" {
				errs = append(errs, "auth.oidc.issuer_url must be set when auth.methods includes oidc")
			}
			if a.OIDC.Audience == "" {
				errs = append(errs, "auth.oidc.audience must be set when auth.methods includes oidc")
			}
			if a.OIDC.JWKSCacheTTL <= 0 {
				errs = append(errs, "auth.oidc.jwks_cache_ttl must be > 0")
			}
			if err := validateAbsoluteURL(a.OIDC.IssuerURL); a.OIDC.IssuerURL != "" && err != "" {
				errs = append(errs, "auth.oidc.issuer_url "+err)
			}
			errs = append(errs, a.OIDC.Introspection.validate("auth.oidc.introspection")...)
		default:
			errs = append(errs, "auth.methods entries must each be one of: api_key, jwt, oidc (got \""+string(m)+"\")")
		}
	}
	if a.OIDC.Introspection.Enabled && !a.Has(AuthMethodOIDC) {
		errs = append(errs, "auth.oidc.introspection.enabled requires auth.methods to include oidc")
	}
	errs = append(errs, a.validateAdmin(a.Admin.Enabled())...)
	return errs
}

// validate checks the introspection settings; key is the config path they
// live under, for error messages.
func (i OIDCIntrospectionConfig) validate(key string) []string {
	if !i.Enabled {
		return nil
	}
	var errs []string
	// The endpoint may legitimately be empty here: it then comes from the
	// provider's discovery document, which is checked at server startup.
	if i.Endpoint != "" {
		if err := validateAbsoluteURL(i.Endpoint); err != "" {
			errs = append(errs, key+".endpoint "+err)
		}
	}
	if i.ClientID == "" {
		errs = append(errs, key+".client_id must be set when introspection is enabled")
	}
	if i.ClientSecret == "" {
		errs = append(errs, key+".client_secret must be set when introspection is enabled")
	}
	if i.CacheTTL <= 0 {
		errs = append(errs, key+".cache_ttl must be > 0")
	}
	if i.NegativeCacheTTL <= 0 {
		errs = append(errs, key+".negative_cache_ttl must be > 0")
	}
	return errs
}

// validateAbsoluteURL returns "" when raw is an absolute http(s) URL with a
// host and no query or fragment, else a description of the problem.
func validateAbsoluteURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "must be an absolute http(s) URL"
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "must not contain a query or fragment"
	}
	return ""
}
