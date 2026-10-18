package config

import (
	"slices"
	"strings"
	"time"

	"github.com/getargvio/argvio/internal/role"
)

// AdminConfig configures admin identities: callers of the metrics server who,
// once recognised from a verified IdP token, may use every active tenant
// without tenant_members rows. The public (ingest) server ignores it.
//
// Admin access is off unless at least one recognition mechanism is
// configured (see Enabled); with none, nothing here has any effect.
type AdminConfig struct {
	OIDC AdminOIDCConfig `koanf:"oidc"`

	// Claim recognises an admin by a claim of the verified token.
	Claim AdminClaimConfig `koanf:"claim"`
	// Subjects recognises an admin by the verified token's (iss, sub).
	Subjects []string `koanf:"subjects"`

	// Role is the role every admin has in every tenant (default viewer).
	// RoleClaim, if set, names a claim of the verified token that may
	// override it. Neither recognises anyone: they only apply once a rule
	// above has.
	Role      string `koanf:"role"`
	RoleClaim string `koanf:"role_claim"`
}

// AdminClaimConfig is the claim recognition rule: a token whose claim Name is
// the string Value, or an array containing it, is an admin. Both or neither
// must be set.
type AdminClaimConfig struct {
	Name  string `koanf:"name"`
	Value string `koanf:"value"`
}

// Enabled reports whether the rule is configured.
func (c AdminClaimConfig) Enabled() bool { return c.Name != "" && c.Value != "" }

// AdminOIDCConfig configures how an admin access token is verified. It
// mirrors OIDCConfig, and every field left unset falls back to the
// corresponding auth.oidc.* value (see Effective), so by default admin tokens
// are verified exactly like user tokens.
type AdminOIDCConfig struct {
	IssuerURL      string                   `koanf:"issuer_url"`
	Audience       string                   `koanf:"audience"`
	RequiredScopes []string                 `koanf:"required_scopes"`
	JWKSCacheTTL   time.Duration            `koanf:"jwks_cache_ttl"`
	Introspection  AdminIntrospectionConfig `koanf:"introspection"`
}

// AdminIntrospectionConfig mirrors OIDCIntrospectionConfig. Enabled is a
// pointer so that "not set" (inherit) differs from an explicit false.
type AdminIntrospectionConfig struct {
	Enabled          *bool         `koanf:"enabled"`
	Endpoint         string        `koanf:"endpoint"`
	ClientID         string        `koanf:"client_id"`
	ClientSecret     string        `koanf:"client_secret"`
	CacheTTL         time.Duration `koanf:"cache_ttl"`
	NegativeCacheTTL time.Duration `koanf:"negative_cache_ttl"`
}

// Enabled reports whether any admin recognition mechanism is configured: the
// claim rule or a non-empty subject allowlist.
func (a AdminConfig) Enabled() bool {
	return a.Claim.Enabled() || len(a.SubjectAllowlist()) > 0
}

// SubjectAllowlist is Subjects as the matcher uses it: entries trimmed, empty
// ones and duplicates dropped, order kept. No wildcard is interpreted.
func (a AdminConfig) SubjectAllowlist() []string {
	var out []string
	for _, s := range a.Subjects {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// EffectiveRole is the role admins have unless RoleClaim says otherwise.
func (a AdminConfig) EffectiveRole() string {
	if a.Role == "" {
		return role.Viewer
	}
	return a.Role
}

// Effective resolves the verification config for admin tokens: base with
// every field set under auth.admin.oidc.* taking precedence.
//
// The introspection endpoint and client credentials are the exception to
// "falls back to auth.oidc": they are inherited only while the admin issuer
// is the same as base's. Otherwise base's client secret would be sent to a
// different provider's endpoint, so a different issuer has to bring its own
// (and introspection stays off unless it enables it).
func (a AdminOIDCConfig) Effective(base OIDCConfig) OIDCConfig {
	eff := base
	eff.RequiredScopes = slices.Clone(base.RequiredScopes)
	if a.IssuerURL != "" {
		eff.IssuerURL = a.IssuerURL
	}
	if a.Audience != "" {
		eff.Audience = a.Audience
	}
	if len(a.RequiredScopes) > 0 {
		eff.RequiredScopes = slices.Clone(a.RequiredScopes)
	}
	if a.JWKSCacheTTL > 0 {
		eff.JWKSCacheTTL = a.JWKSCacheTTL
	}

	in := a.Introspection
	if eff.IssuerURL != base.IssuerURL {
		eff.Introspection.Enabled = false
		eff.Introspection.Endpoint = ""
		eff.Introspection.ClientID = ""
		eff.Introspection.ClientSecret = ""
	}
	if in.Enabled != nil {
		eff.Introspection.Enabled = *in.Enabled
	}
	if in.Endpoint != "" {
		eff.Introspection.Endpoint = in.Endpoint
	}
	if in.ClientID != "" {
		eff.Introspection.ClientID = in.ClientID
	}
	if in.ClientSecret != "" {
		eff.Introspection.ClientSecret = in.ClientSecret
	}
	if in.CacheTTL > 0 {
		eff.Introspection.CacheTTL = in.CacheTTL
	}
	if in.NegativeCacheTTL > 0 {
		eff.Introspection.NegativeCacheTTL = in.NegativeCacheTTL
	}
	return eff
}

// SameVerification reports whether tokens are verified identically under o:
// same issuer, audience, scopes and introspection setup. Admin tokens then
// need no verifier (and no JWKS or introspection cache) of their own.
func (c OIDCConfig) SameVerification(o OIDCConfig) bool {
	return c.IssuerURL == o.IssuerURL &&
		c.Audience == o.Audience &&
		c.JWKSCacheTTL == o.JWKSCacheTTL &&
		c.RequireAccessTokenTyp == o.RequireAccessTokenTyp &&
		slices.Equal(c.RequiredScopes, o.RequiredScopes) &&
		c.Introspection == o.Introspection
}

func (a AdminOIDCConfig) isZero() bool {
	in := a.Introspection
	return a.IssuerURL == "" && a.Audience == "" && len(a.RequiredScopes) == 0 && a.JWKSCacheTTL == 0 &&
		in.Enabled == nil && in.Endpoint == "" && in.ClientID == "" && in.ClientSecret == "" &&
		in.CacheTTL == 0 && in.NegativeCacheTTL == 0
}

// validateAdmin checks auth.admin.*. recognition says whether a recognition
// mechanism is configured (AdminConfig.Enabled); it is a parameter so the
// checks that only apply once admin is on stay testable.
func (a AuthConfig) validateAdmin(recognition bool) []string {
	var errs []string
	if (a.Admin.Claim.Name == "") != (a.Admin.Claim.Value == "") {
		errs = append(errs, "auth.admin.claim.name and auth.admin.claim.value must be set together (or both left empty)")
	}
	if r := a.Admin.EffectiveRole(); !role.Known(r) {
		errs = append(errs, "auth.admin.role must be one of: "+role.Viewer+", "+role.Admin+" (got \""+r+"\")")
	}

	if !recognition {
		// Settings that can never take effect are a mistake worth failing
		// on, not a silent no-op: the operator expects admin access.
		if !a.Admin.OIDC.isZero() || a.Admin.RoleClaim != "" || a.Admin.EffectiveRole() != role.Viewer {
			errs = append(errs, "auth.admin.oidc.*, role or role_claim is set but no admin recognition mechanism is configured, so admin access would stay off; configure one or remove these settings")
		}
		return errs
	}

	if !a.Has(AuthMethodOIDC) {
		errs = append(errs, "auth.admin requires auth.methods to include oidc")
	}
	eff := a.Admin.OIDC.Effective(a.OIDC)
	if eff.IssuerURL == "" {
		errs = append(errs, "auth.admin.oidc.issuer_url (or auth.oidc.issuer_url) must be set when admin access is enabled")
	} else if err := validateAbsoluteURL(eff.IssuerURL); err != "" {
		errs = append(errs, "auth.admin.oidc.issuer_url "+err)
	}
	if eff.Audience == "" {
		errs = append(errs, "auth.admin.oidc.audience (or auth.oidc.audience) must be set when admin access is enabled")
	}
	if eff.JWKSCacheTTL <= 0 {
		errs = append(errs, "auth.admin.oidc.jwks_cache_ttl must be > 0")
	}
	errs = append(errs, eff.Introspection.validate("auth.admin.oidc.introspection")...)
	return errs
}
