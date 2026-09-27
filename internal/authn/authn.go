// Package authn resolves caller identity against the configured auth
// methods: api_key, jwt, or oidc.
package authn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

var ErrUnauthenticated = errors.New("authn: invalid or missing credentials")
var ErrTenantSuspended = errors.New("authn: tenant suspended")

// Identity is the authenticated caller. Resolved.APIKey is zero unless
// Method is MethodAPIKey; Claims is nil for MethodAPIKey.
type Identity struct {
	Method   Method
	Resolved *tenant.Resolved
	Claims   jwt.MapClaims
}

// Verifier authenticates bearer tokens against the configured methods.
type Verifier struct {
	methods       map[Method]bool
	jwtSigningKey []byte
	oidcVerifier  *oidc.Verifier
	resolver      tenant.Resolver
	apiKeyScope   string
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
		v.oidcVerifier = oidc.NewVerifier(cfg.OIDC.IssuerURL, cfg.OIDC.Audience, cfg.OIDC.RequiredScopes, cfg.OIDC.JWKSCacheTTL)
	}
	return v
}

// Authenticate picks the method structurally, not by trying each in turn:
// an opaque token is an API key; a JWT-shaped token's alg picks jwt vs oidc.
func (v *Verifier) Authenticate(ctx context.Context, raw string) (Identity, error) {
	if raw == "" {
		return Identity{}, ErrUnauthenticated
	}

	if alg, ok := peekAlg(raw); ok {
		switch {
		case alg == "HS256" && v.methods[MethodJWT]:
			return v.authenticateJWT(ctx, raw)
		case isAsymmetricAlg(alg) && v.methods[MethodOIDC]:
			return v.authenticateOIDC(ctx, raw)
		default:
			return Identity{}, ErrUnauthenticated
		}
	}

	if v.methods[MethodAPIKey] {
		return v.authenticateAPIKey(ctx, raw)
	}
	return Identity{}, ErrUnauthenticated
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

func (v *Verifier) authenticateOIDC(ctx context.Context, raw string) (Identity, error) {
	claims, err := v.oidcVerifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	return v.identityFromClaims(ctx, MethodOIDC, claims)
}

// identityFromClaims re-resolves the tenant rather than trusting the claims alone.
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
	if resolved.Tenant.Status != "active" {
		return Identity{}, ErrTenantSuspended
	}
	return Identity{Method: method, Resolved: resolved, Claims: claims}, nil
}

func (v *Verifier) authenticateAPIKey(ctx context.Context, raw string) (Identity, error) {
	resolved, found, err := v.resolver.Resolve(ctx, raw, v.apiKeyScope)
	if err != nil {
		return Identity{}, err
	}
	if !found || resolved.APIKey.RevokedAt != nil {
		return Identity{}, ErrUnauthenticated
	}
	if resolved.Tenant.Status != "active" {
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
