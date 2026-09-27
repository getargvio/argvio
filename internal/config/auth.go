package config

import "time"

type AuthMethod string

const (
	AuthMethodAPIKey AuthMethod = "api_key" // scoped tenant API key
	AuthMethodJWT    AuthMethod = "jwt"     // dashboard-user session/JWT (HS256)
	AuthMethodOIDC   AuthMethod = "oidc"    // third-party OIDC provider (Auth0, Okta, Azure AD, ...)
)

// OIDCConfig configures third-party OIDC token verification.
type OIDCConfig struct {
	IssuerURL      string        `koanf:"issuer_url"`
	Audience       string        `koanf:"audience"`
	RequiredScopes []string      `koanf:"required_scopes"`
	JWKSCacheTTL   time.Duration `koanf:"jwks_cache_ttl"`
}

// AuthConfig is the global auth config shared by every server.
type AuthConfig struct {
	Methods []AuthMethod `koanf:"methods"`

	JWTSigningKey string     `koanf:"jwt_signing_key"`
	OIDC          OIDCConfig `koanf:"oidc"`
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
		default:
			errs = append(errs, "auth.methods entries must each be one of: api_key, jwt, oidc (got \""+string(m)+"\")")
		}
	}
	return errs
}
