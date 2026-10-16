// Package oidctest is a fake OIDC provider for tests: discovery, an RS256
// JWKS and an RFC 7662 introspection endpoint, with request counters and
// injectable failures.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Audience is the resource identifier the provider's tokens are minted for.
const Audience = "https://api.example.com"

// ClientID, ClientSecret are the credentials the introspection endpoint wants.
const (
	ClientID     = "argvio-api"
	ClientSecret = "api-secret"
)

type Provider struct {
	Server *httptest.Server
	Key    *rsa.PrivateKey
	KID    string

	// Hit counters, per endpoint.
	DiscoveryHits, JWKSHits, IntrospectHits atomic.Int32
	// A non-zero status makes that endpoint fail with it.
	JWKSStatus, IntrospectStatus atomic.Int32

	mu     sync.Mutex
	opaque map[string]map[string]any
}

func New(t *testing.T) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{Key: key, KID: "k1", opaque: map[string]map[string]any{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		p.DiscoveryHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 p.Issuer(),
			"jwks_uri":               p.Server.URL + "/jwks.json",
			"introspection_endpoint": p.Server.URL + "/introspect",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		p.JWKSHits.Add(1)
		if st := int(p.JWKSStatus.Load()); st != 0 {
			http.Error(w, "boom", st)
			return
		}
		e := p.Key.E
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": p.KID, "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(p.Key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(e)).Bytes()),
		}}})
	})
	mux.HandleFunc("/introspect", func(w http.ResponseWriter, r *http.Request) {
		p.IntrospectHits.Add(1)
		if st := int(p.IntrospectStatus.Load()); st != 0 {
			http.Error(w, "boom", st)
			return
		}
		if id, secret, ok := r.BasicAuth(); !ok || id != ClientID || secret != ClientSecret {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		_ = r.ParseForm()
		p.mu.Lock()
		resp, ok := p.opaque[r.PostForm.Get("token")]
		p.mu.Unlock()
		if !ok {
			resp = map[string]any{"active": false}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Server.Close)
	return p
}

// Issuer is the issuer string the discovery document states.
func (p *Provider) Issuer() string { return p.Server.URL }

// Claims returns valid access-token claims for sub; callers adjust them.
func (p *Provider) Claims(sub string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   p.Issuer(),
		"aud":   Audience,
		"sub":   sub,
		"scope": "openid metrics:read",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
}

// Sign signs claims as an RS256 JWT.
func (p *Provider) Sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = p.KID
	s, err := tok.SignedString(p.Key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// AddOpaque makes the introspection endpoint answer active for token with the
// claims for sub (adjusted by mutate, which may be nil).
func (p *Provider) AddOpaque(token, sub string, mutate func(map[string]any)) {
	resp := map[string]any{"active": true}
	for k, v := range p.Claims(sub) {
		resp[k] = v
	}
	if mutate != nil {
		mutate(resp)
	}
	p.mu.Lock()
	p.opaque[token] = resp
	p.mu.Unlock()
}
