// Package authn resolves caller identity against the configured auth
// methods: api_key, jwt, or oidc (JWT access tokens and, via introspection,
// opaque ones).
package authn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/oidc"
	"github.com/getargvio/argvio/internal/tenant"
)

type Method string

const (
	MethodAPIKey Method = "api_key"
	MethodJWT    Method = "jwt"
	MethodOIDC   Method = "oidc"
)

// Authentication failures, by how a caller should react: ErrUnauthenticated
// means "present a (new) credential"; everything else means the credential
// was understood and the answer is no (or, for ErrProviderUnavailable, "not
// now"), so re-authenticating would not help.
var (
	ErrUnauthenticated = errors.New("authn: invalid or missing credentials")
	ErrTenantSuspended = errors.New("authn: tenant suspended")

	// ErrNoTenantAccess: authenticated, but not a member of any tenant, or of
	// the one the caller asked for.
	ErrNoTenantAccess = errors.New("authn: no access to the requested tenant")
	// ErrTenantRequired: the identity belongs to several tenants and the
	// request did not say which.
	ErrTenantRequired = errors.New("authn: tenant selection required")

	// ErrInsufficientScope and ErrProviderUnavailable are the OIDC verifier's,
	// passed through so callers can errors.Is them without importing oidc.
	ErrInsufficientScope   = oidc.ErrInsufficientScope
	ErrProviderUnavailable = oidc.ErrProviderUnavailable
)

// Identity is the authenticated caller. Resolved.APIKey is zero unless
// Method is MethodAPIKey; Claims is nil for MethodAPIKey.
//
// For MethodOIDC, Issuer and Subject are the verified token's identity and
// Role is the caller's role in the resolved tenant. Resolved is nil on an
// Identity from AuthenticateSubject, which stops before choosing a tenant.
type Identity struct {
	Method   Method
	Resolved *tenant.Resolved
	Claims   jwt.MapClaims

	Issuer  string
	Subject string
	Role    string
}

// Verifier authenticates bearer tokens against the configured methods.
type Verifier struct {
	methods       map[Method]bool
	jwtSigningKey []byte
	oidcVerifier  *oidc.Verifier
	resolver      tenant.Resolver
	apiKeyScope   string

	// trustTenantIDClaim lets an OIDC token's tenant_id claim pick the tenant
	// without a membership row. Off unless explicitly configured.
	trustTenantIDClaim bool
}

// New builds a Verifier. apiKeyScope must never overlap between the public
// ingest and metrics-query servers.
func New(cfg config.AuthConfig, resolver tenant.Resolver, apiKeyScope string) *Verifier {
	methods := make(map[Method]bool, len(cfg.Methods))
	for _, m := range cfg.Methods {
		methods[Method(m)] = true
	}
	v := &Verifier{
		methods:       methods,
		jwtSigningKey: []byte(cfg.JWTSigningKey),
		resolver:      resolver,
		apiKeyScope:   apiKeyScope,
	}
	if methods[MethodOIDC] {
		ov := oidc.NewVerifier(cfg.OIDC.IssuerURL, cfg.OIDC.Audience, cfg.OIDC.RequiredScopes, cfg.OIDC.JWKSCacheTTL)
		ov.RequireAccessTokenTyp = cfg.OIDC.RequireAccessTokenTyp
		ov.Introspection = oidc.IntrospectionConfig{
			Enabled:          cfg.OIDC.Introspection.Enabled,
			Endpoint:         cfg.OIDC.Introspection.Endpoint,
			ClientID:         cfg.OIDC.Introspection.ClientID,
			ClientSecret:     cfg.OIDC.Introspection.ClientSecret,
			CacheTTL:         cfg.OIDC.Introspection.CacheTTL,
			NegativeCacheTTL: cfg.OIDC.Introspection.NegativeCacheTTL,
		}
		v.oidcVerifier = ov
		v.trustTenantIDClaim = cfg.OIDC.TrustTenantIDClaim
	}
	return v
}

// CheckStartup fails fast on an auth configuration the identity provider
// cannot honour (e.g. introspection enabled but no endpoint to call).
func (v *Verifier) CheckStartup(ctx context.Context) error {
	if v.oidcVerifier == nil {
		return nil
	}
	return v.oidcVerifier.CheckStartup(ctx)
}

// Authenticate resolves a bearer credential to an identity with no tenant
// selection, which suits API keys and callers that belong to one tenant.
func (v *Verifier) Authenticate(ctx context.Context, raw string) (Identity, error) {
	return v.AuthenticateForTenant(ctx, raw, "")
}

// AuthenticateForTenant is Authenticate for a caller that may name the tenant
// it wants (the X-Argvio-Tenant header). The hint only ever chooses among
// tenants the verified identity already owns; it never grants access, so a
// hint naming any other tenant is ErrNoTenantAccess.
func (v *Verifier) AuthenticateForTenant(ctx context.Context, raw, tenantHint string) (Identity, error) {
	id, err := v.AuthenticateSubject(ctx, raw)
	if err != nil {
		return Identity{}, err
	}
	if id.Resolved != nil { // api_key / jwt: bound to one tenant by the credential
		if tenantHint != "" && !tenantMatches(tenantHint, id.Resolved.Tenant.ID) {
			return Identity{}, ErrNoTenantAccess
		}
		return id, nil
	}
	return v.selectTenant(ctx, id, tenantHint)
}

// AuthenticateSubject verifies the credential without choosing a tenant. For
// an OIDC credential the result carries the verified Issuer/Subject and a nil
// Resolved; api_key and jwt credentials already name their tenant, so those
// come back resolved.
//
// The method is picked structurally, not by trying each in turn: an `argv_`
// prefix is an API key; a three-part JWS goes to jwt or oidc by its alg; any
// other token is opaque and goes to introspection when that is enabled, and
// is otherwise treated as an API key.
func (v *Verifier) AuthenticateSubject(ctx context.Context, raw string) (Identity, error) {
	if raw == "" {
		return Identity{}, ErrUnauthenticated
	}

	if strings.HasPrefix(raw, tenant.APIKeyPrefix) {
		return v.authenticateAPIKey(ctx, raw)
	}

	if alg, ok := peekAlg(raw); ok {
		switch {
		case alg == "HS256" && v.methods[MethodJWT]:
			return v.authenticateJWT(ctx, raw)
		case isAsymmetricAlg(alg) && v.methods[MethodOIDC]:
			return v.authenticateOIDC(ctx, raw, v.oidcVerifier.Verify)
		default:
			return Identity{}, ErrUnauthenticated
		}
	}

	if v.methods[MethodOIDC] && v.oidcVerifier.IntrospectionEnabled() {
		return v.authenticateOIDC(ctx, raw, v.oidcVerifier.Introspect)
	}
	return v.authenticateAPIKey(ctx, raw)
}

// authenticateJWT pins HS256 to rule out algorithm-confusion attacks.
func (v *Verifier) authenticateJWT(ctx context.Context, raw string) (Identity, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		return v.jwtSigningKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	return v.identityFromClaims(ctx, MethodJWT, claims)
}

// authenticateOIDC verifies a token with verify (JWT or introspection) and
// translates the verifier's error classes.
func (v *Verifier) authenticateOIDC(ctx context.Context, raw string, verify func(context.Context, string) (jwt.MapClaims, error)) (Identity, error) {
	claims, err := verify(ctx, raw)
	switch {
	case err == nil:
	case errors.Is(err, oidc.ErrInvalidToken):
		// Keep the reason for the operator's log; callers match on the sentinel.
		return Identity{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	default:
		return Identity{}, err // insufficient scope / provider unavailable
	}
	// The verifier checked iss against its configured issuer, which is what
	// memberships are keyed on; do not take it from a response that may omit it.
	sub, _ := claims["sub"].(string)
	return Identity{Method: MethodOIDC, Claims: claims, Issuer: v.oidcVerifier.IssuerURL, Subject: sub}, nil
}

// Memberships lists the tenants an OIDC identity belongs to.
func (v *Verifier) Memberships(ctx context.Context, id Identity) ([]tenant.Membership, error) {
	if id.Method != MethodOIDC || id.Subject == "" {
		return nil, nil
	}
	return v.resolver.Memberships(ctx, id.Issuer, id.Subject)
}

// selectTenant maps a verified OIDC identity to exactly one tenant it is a
// member of. The tenant scope derives from the result only; the hint merely
// chooses among memberships.
func (v *Verifier) selectTenant(ctx context.Context, id Identity, hint string) (Identity, error) {
	// A tenant_id claim is never authority by itself. Unless explicitly
	// trusted it must name a membership.
	claimTenant, hasClaim, err := tenantClaim(id.Claims)
	if err != nil {
		return Identity{}, ErrNoTenantAccess
	}
	if hasClaim && v.trustTenantIDClaim {
		if hint != "" && !tenantMatches(hint, claimTenant) {
			return Identity{}, ErrNoTenantAccess
		}
		return v.resolveTenant(ctx, id, claimTenant, "")
	}

	if id.Subject == "" {
		return Identity{}, ErrNoTenantAccess
	}
	members, err := v.resolver.Memberships(ctx, id.Issuer, id.Subject)
	if err != nil {
		return Identity{}, err
	}
	if hasClaim && !hasMembership(members, claimTenant) {
		return Identity{}, ErrNoTenantAccess
	}

	var chosen *tenant.Membership
	switch {
	case hint != "":
		for i := range members {
			if tenantMatches(hint, members[i].Tenant.ID) {
				chosen = &members[i]
				break
			}
		}
		if chosen == nil {
			return Identity{}, ErrNoTenantAccess
		}
	case len(members) == 0:
		return Identity{}, ErrNoTenantAccess
	case len(members) > 1:
		return Identity{}, ErrTenantRequired
	default:
		chosen = &members[0]
	}
	return v.resolveTenant(ctx, id, chosen.Tenant.ID, chosen.Role)
}

func (v *Verifier) resolveTenant(ctx context.Context, id Identity, tenantID uuid.UUID, role string) (Identity, error) {
	resolved, found, err := v.resolver.ResolveByTenantID(ctx, tenantID)
	if err != nil {
		return Identity{}, err
	}
	if !found {
		return Identity{}, ErrNoTenantAccess
	}
	if resolved.Tenant.Status != tenant.StatusActive {
		return Identity{}, ErrTenantSuspended
	}
	id.Resolved, id.Role = resolved, role
	return id, nil
}

func hasMembership(members []tenant.Membership, tenantID uuid.UUID) bool {
	for _, m := range members {
		if m.Tenant.ID == tenantID {
			return true
		}
	}
	return false
}

// tenantMatches compares a client-supplied tenant UUID to a real one. An
// unparseable value matches nothing.
func tenantMatches(hint string, tenantID uuid.UUID) bool {
	h, err := uuid.Parse(hint)
	return err == nil && h == tenantID
}

// tenantClaim reads the optional tenant_id claim. A claim that is present but
// malformed is an error: it cannot be a membership.
func tenantClaim(claims jwt.MapClaims) (id uuid.UUID, present bool, err error) {
	raw, ok := claims["tenant_id"]
	if !ok {
		return uuid.Nil, false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return uuid.Nil, true, errors.New("authn: tenant_id claim is not a string")
	}
	id, err = uuid.Parse(s)
	return id, true, err
}

// identityFromClaims is the legacy jwt method's resolution: it re-resolves the
// tenant named by the claim rather than trusting the claims alone.
func (v *Verifier) identityFromClaims(ctx context.Context, method Method, claims jwt.MapClaims) (Identity, error) {
	tid, err := tenantIDFromClaims(claims)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	resolved, found, err := v.resolver.ResolveByTenantID(ctx, tid)
	if err != nil {
		return Identity{}, err
	}
	if !found {
		return Identity{}, ErrUnauthenticated
	}
	if resolved.Tenant.Status != tenant.StatusActive {
		return Identity{}, ErrTenantSuspended
	}
	return Identity{Method: method, Resolved: resolved, Claims: claims}, nil
}

func (v *Verifier) authenticateAPIKey(ctx context.Context, raw string) (Identity, error) {
	if !v.methods[MethodAPIKey] {
		return Identity{}, ErrUnauthenticated
	}
	resolved, found, err := v.resolver.Resolve(ctx, raw, v.apiKeyScope)
	if err != nil {
		return Identity{}, err
	}
	if !found || resolved.APIKey.RevokedAt != nil {
		return Identity{}, ErrUnauthenticated
	}
	if resolved.Tenant.Status != tenant.StatusActive {
		return Identity{}, ErrTenantSuspended
	}
	return Identity{Method: MethodAPIKey, Resolved: resolved}, nil
}

func tenantIDFromClaims(claims jwt.MapClaims) (uuid.UUID, error) {
	tidRaw, ok := claims["tenant_id"]
	if !ok {
		return uuid.Nil, errors.New("authn: token missing tenant_id claim")
	}
	tidStr, ok := tidRaw.(string)
	if !ok {
		return uuid.Nil, errors.New("authn: token tenant_id claim is not a string")
	}
	return uuid.Parse(tidStr)
}

// peekAlg is unverified, used only to route; the chosen verifier re-checks alg.
func peekAlg(raw string) (string, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return "", false
	}
	return header.Alg, true
}

func isAsymmetricAlg(alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512":
		return true
	default:
		return false
	}
}
