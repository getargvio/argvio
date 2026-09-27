// Package oidc verifies bearer tokens against a third-party OIDC provider.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// allowedSigningMethods excludes "none" and HMAC to rule out algorithm-confusion attacks.
var allowedSigningMethods = []string{
	"RS256", "RS384", "RS512",
	"PS256", "PS384", "PS512",
	"ES256", "ES384", "ES512",
}

type discovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

const maxDiscoveryBodyBytes = 1 << 20 // 1 MiB

// Verifier validates bearer tokens issued by a single OIDC provider,
// lazily fetching and caching discovery + JWKS.
type Verifier struct {
	IssuerURL      string
	Audience       string
	RequiredScopes []string
	CacheTTL       time.Duration
	HTTPClient     *http.Client

	mu sync.Mutex
	kf keyfunc.Keyfunc
}

func NewVerifier(issuerURL, audience string, requiredScopes []string, cacheTTL time.Duration) *Verifier {
	return &Verifier{
		IssuerURL:      strings.TrimSuffix(issuerURL, "/"),
		Audience:       audience,
		RequiredScopes: requiredScopes,
		CacheTTL:       cacheTTL,
		HTTPClient:     &http.Client{Timeout: 10 * time.Second},
	}
}

func (v *Verifier) client() *http.Client {
	if v.HTTPClient != nil {
		return v.HTTPClient
	}
	return http.DefaultClient
}

// keyfunc lazily discovers the provider's jwks_uri and builds a keyfunc.Keyfunc
// backed by it, rejecting a discovery issuer that doesn't match v.IssuerURL.
// The result is cached for the lifetime of the Verifier: the underlying
// keyfunc.Keyfunc refreshes the JWKS itself on CacheTTL and on unknown kid.
func (v *Verifier) ensureKeyfunc(ctx context.Context) (keyfunc.Keyfunc, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.kf != nil {
		return v.kf, nil
	}

	var d discovery
	if err := fetchJSON(ctx, v.client(), v.IssuerURL+"/.well-known/openid-configuration", &d); err != nil {
		return nil, fmt.Errorf("oidc: fetching discovery document: %w", err)
	}
	if d.Issuer != v.IssuerURL {
		return nil, fmt.Errorf("oidc: discovery issuer %q does not match configured issuer %q", d.Issuer, v.IssuerURL)
	}
	if d.JWKSURI == "" {
		return nil, errors.New("oidc: discovery document has no jwks_uri")
	}

	kf, err := keyfunc.NewDefaultOverrideCtx(context.Background(), []string{d.JWKSURI}, keyfunc.Override{
		Client:          v.client(),
		RefreshInterval: v.CacheTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("oidc: creating JWKS client: %w", err)
	}
	v.kf = kf
	return kf, nil
}

func fetchJSON(ctx context.Context, client *http.Client, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxDiscoveryBodyBytes)).Decode(dst)
}

func (v *Verifier) Verify(ctx context.Context, rawToken string) (jwt.MapClaims, error) {
	kf, err := v.ensureKeyfunc(ctx)
	if err != nil {
		return nil, fmt.Errorf("oidc: resolving JWKS: %w", err)
	}

	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(rawToken, claims, kf.KeyfuncCtx(ctx),
		jwt.WithValidMethods(allowedSigningMethods), jwt.WithIssuer(v.IssuerURL), jwt.WithAudience(v.Audience))
	if err != nil {
		return nil, fmt.Errorf("oidc: invalid token: %w", err)
	}

	if err := v.checkScopes(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// checkScopes reads scopes from either the "scope" or "scp" (Azure AD) claim.
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
	if arr, ok := claims["scp"].([]any); ok {
		for _, sc := range arr {
			if s, ok := sc.(string); ok {
				granted[s] = true
			}
		}
	}

	var missing []string
	for _, req := range v.RequiredScopes {
		if !granted[req] {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("oidc: token missing required scope(s): %s", strings.Join(missing, ", "))
	}
	return nil
}
