package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testProvider spins up a fake OIDC provider: /.well-known/openid-configuration
// + a JWKS endpoint, backed by a freshly generated RSA key pair.
type testProvider struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newTestProvider(t *testing.T) *testProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &testProvider{key: key, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":   p.issuer(),
			"jwks_uri": p.issuer() + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": p.kid,
					"use": "sig",
					"n":   base64.RawURLEncoding.EncodeToString(p.key.PublicKey.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(bigEndianExponent(p.key.PublicKey.E)),
				},
			},
		})
	})
	p.srv = httptest.NewServer(mux)
	return p
}

func bigEndianExponent(e int) []byte {
	b := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return b[i:]
}

func (p *testProvider) issuer() string { return p.srv.URL }

func (p *testProvider) close() { p.srv.Close() }

func (p *testProvider) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = p.kid
	s, err := tok.SignedString(p.key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func baseClaims(p *testProvider, audience string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":       p.issuer(),
		"aud":       audience,
		"sub":       "user-1",
		"tenant_id": "11111111-1111-1111-1111-111111111111",
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
}

func TestVerifier_ValidToken(t *testing.T) {
	p := newTestProvider(t)
	defer p.close()

	v := NewVerifier(p.issuer(), "argvio-metrics", nil, time.Minute)
	token := p.sign(t, baseClaims(p, "argvio-metrics"))

	claims, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims["tenant_id"] != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("tenant_id = %v", claims["tenant_id"])
	}
}

func TestVerifier_WrongAudience(t *testing.T) {
	p := newTestProvider(t)
	defer p.close()

	v := NewVerifier(p.issuer(), "argvio-metrics", nil, time.Minute)
	token := p.sign(t, baseClaims(p, "someone-else"))

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestVerifier_ExpiredToken(t *testing.T) {
	p := newTestProvider(t)
	defer p.close()

	v := NewVerifier(p.issuer(), "argvio-metrics", nil, time.Minute)
	claims := baseClaims(p, "argvio-metrics")
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	token := p.sign(t, claims)

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestVerifier_RejectsHMACAlgConfusion(t *testing.T) {
	p := newTestProvider(t)
	defer p.close()

	v := NewVerifier(p.issuer(), "argvio-metrics", nil, time.Minute)

	// Sign with HS256 using the RSA public key's modulus as the "secret" —
	// the classic algorithm-confusion attack against public-key verifiers.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, baseClaims(p, "argvio-metrics"))
	tok.Header["kid"] = p.kid
	token, err := tok.SignedString(p.key.PublicKey.N.Bytes())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("expected error for HS256 alg-confusion token")
	}
}

func TestVerifier_RequiredScopes(t *testing.T) {
	p := newTestProvider(t)
	defer p.close()

	v := NewVerifier(p.issuer(), "argvio-metrics", []string{"metrics:read"}, time.Minute)

	t.Run("missing scope is rejected", func(t *testing.T) {
		token := p.sign(t, baseClaims(p, "argvio-metrics"))
		if _, err := v.Verify(context.Background(), token); err == nil {
			t.Fatal("expected error for missing scope")
		}
	})

	t.Run("scope claim satisfies requirement", func(t *testing.T) {
		claims := baseClaims(p, "argvio-metrics")
		claims["scope"] = "openid metrics:read profile"
		token := p.sign(t, claims)
		if _, err := v.Verify(context.Background(), token); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})

	t.Run("scp claim satisfies requirement", func(t *testing.T) {
		claims := baseClaims(p, "argvio-metrics")
		claims["scp"] = []any{"metrics:read"}
		token := p.sign(t, claims)
		if _, err := v.Verify(context.Background(), token); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
}

func TestVerifier_KeyRotation(t *testing.T) {
	p := newTestProvider(t)
	defer p.close()

	v := NewVerifier(p.issuer(), "argvio-metrics", nil, time.Hour)
	token := p.sign(t, baseClaims(p, "argvio-metrics"))
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify (before rotation): %v", err)
	}

	// Rotate to a new key/kid on the provider; even though the verifier's
	// cache TTL hasn't elapsed, an unknown kid should trigger a refresh.
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p.key = newKey
	p.kid = "test-key-2"

	rotatedToken := p.sign(t, baseClaims(p, "argvio-metrics"))
	if _, err := v.Verify(context.Background(), rotatedToken); err != nil {
		t.Fatalf("Verify (after rotation): %v", err)
	}
}
