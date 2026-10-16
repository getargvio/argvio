// Package oidc verifies bearer tokens against a third-party OIDC provider:
// JWT access tokens against its published JWKS, opaque ones by RFC 7662
// introspection. It relies only on OIDC discovery metadata and the RFCs
// (6750, 7519, 7662, 9068), nothing provider-specific.
package oidc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

// allowedSigningMethods excludes "none" and HMAC to rule out algorithm-confusion attacks.
var allowedSigningMethods = []string{
	"RS256", "RS384", "RS512",
	"PS256", "PS384", "PS512",
	"ES256", "ES384", "ES512",
}

const (
	// clockSkewLeeway absorbs clock drift between us and the provider for
	// exp/nbf, so a freshly minted token is not rejected as "not yet valid".
	clockSkewLeeway = 30 * time.Second

	// defaultUnknownKIDCooldown is the minimum gap between JWKS refreshes
	// triggered by a token naming an unknown kid.
	defaultUnknownKIDCooldown = 10 * time.Second
)

// Verifier validates bearer tokens issued by a single OIDC provider,
// lazily fetching and caching discovery + JWKS.
type Verifier struct {
	// IssuerURL is compared exactly (no trailing-slash normalisation) against
	// the discovery document's `issuer` and each token's `iss`.
	IssuerURL      string
	Audience       string
	RequiredScopes []string
	// CacheTTL is how long a fetched JWKS is trusted before a refresh.
	CacheTTL   time.Duration
	HTTPClient *http.Client

	// RequireAccessTokenTyp rejects a JWT whose header has a `typ` other than
	// at+jwt (RFC 9068).
	RequireAccessTokenTyp bool

	// Introspection configures opaque-token verification (RFC 7662).
	Introspection IntrospectionConfig

	unknownKIDCooldown time.Duration
	now                func() time.Time

	metaMu     sync.RWMutex
	meta       *discovery
	metaFlight singleflight.Group

	jwks        jwksCache
	introCache  introspectionCache
	introFlight singleflight.Group
}

func NewVerifier(issuerURL, audience string, requiredScopes []string, cacheTTL time.Duration) *Verifier {
	return &Verifier{
		IssuerURL:          issuerURL,
		Audience:           audience,
		RequiredScopes:     requiredScopes,
		CacheTTL:           cacheTTL,
		HTTPClient:         &http.Client{Timeout: fetchTimeout},
		unknownKIDCooldown: defaultUnknownKIDCooldown,
		now:                time.Now,
	}
}

// Verify validates a JWT access token. Errors wrap exactly one of
// ErrInvalidToken, ErrInsufficientScope (*ScopeError) or ErrProviderUnavailable.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (jwt.MapClaims, error) {
	// Header checks first: they need no provider round trip, so a malformed
	// or wrong-typed token is a 401 even while the provider is down.
	header, err := peekHeader(rawToken)
	if err != nil {
		return nil, err
	}
	if err := v.checkHeader(header); err != nil {
		return nil, err
	}

	meta, err := v.discover(ctx)
	if err != nil {
		return nil, err
	}

	claims := jwt.MapClaims{}
	_, err = jwt.NewParser(
		jwt.WithValidMethods(allowedSigningMethods),
		jwt.WithIssuer(v.IssuerURL),
		jwt.WithAudience(v.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(clockSkewLeeway),
		jwt.WithTimeFunc(v.now),
	).ParseWithClaims(rawToken, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		alg, _ := t.Header["alg"].(string)
		k, err := v.lookupKey(ctx, meta.JWKSURI, kid, alg)
		if err != nil {
			return nil, err
		}
		return k.key, nil
	})
	if err != nil {
		if errors.Is(err, ErrProviderUnavailable) {
			return nil, err
		}
		return nil, invalidf("%v", err)
	}

	if err := v.checkScopes(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// peekHeader decodes the unverified JOSE header.
func peekHeader(rawToken string) (map[string]any, error) {
	tok, _, err := jwt.NewParser().ParseUnverified(rawToken, jwt.MapClaims{})
	if err != nil {
		return nil, invalidf("malformed token: %v", err)
	}
	return tok.Header, nil
}

func (v *Verifier) checkHeader(header map[string]any) error {
	alg, _ := header["alg"].(string)
	allowed := false
	for _, m := range allowedSigningMethods {
		if m == alg {
			allowed = true
			break
		}
	}
	if !allowed {
		return invalidf("signing algorithm %q is not accepted", alg)
	}

	// An ID token is not an access token; if the provider marks the type,
	// insist on RFC 9068's at+jwt. A token with no `typ` at all is left to
	// the audience check.
	if typ, ok := header["typ"]; ok && v.RequireAccessTokenTyp {
		s, _ := typ.(string)
		// RFC 7515 §4.1.9: media types compare case-insensitively and the
		// "application/" prefix is optional.
		if strings.TrimPrefix(strings.ToLower(s), "application/") != "at+jwt" {
			return invalidf("token typ %q is not at+jwt", s)
		}
	}
	return nil
}

// checkScopes reads scopes from either the "scope" (RFC 6749/9068/7662, a
// space-delimited string) or "scp" (array) claim.
func (v *Verifier) checkScopes(claims jwt.MapClaims) error {
	if len(v.RequiredScopes) == 0 {
		return nil
	}
	granted := make(map[string]bool)
	if s, ok := claims["scope"].(string); ok {
		for _, sc := range strings.Fields(s) {
			granted[sc] = true
		}
	}
	switch scp := claims["scp"].(type) {
	case []any:
		for _, sc := range scp {
			if s, ok := sc.(string); ok {
				granted[s] = true
			}
		}
	case string:
		for _, sc := range strings.Fields(scp) {
			granted[sc] = true
		}
	}

	var missing []string
	for _, req := range v.RequiredScopes {
		if !granted[req] {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return &ScopeError{Missing: missing}
	}
	return nil
}
