package authn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/oidc/oidctest"
	"github.com/getargvio/argvio/internal/tenant"
	"github.com/getargvio/argvio/internal/tenant/tenanttest"
)

const (
	apiKey      = "argv_live_qry_abcdef"
	opaqueToken = "opaque-token-123"
	jwtSecret   = "hs256-secret"
)

type fixture struct {
	idp *oidctest.Provider
	res *tenanttest.Resolver
}

func newFixture(t *testing.T) *fixture {
	return &fixture{idp: oidctest.New(t), res: tenanttest.New()}
}

func (f *fixture) cfg(methods []config.AuthMethod, introspect bool, mutate func(*config.AuthConfig)) config.AuthConfig {
	c := config.AuthConfig{
		Methods:       methods,
		JWTSigningKey: jwtSecret,
		OIDC: config.OIDCConfig{
			IssuerURL: f.idp.Issuer(), Audience: oidctest.Audience, JWKSCacheTTL: time.Hour,
			Introspection: config.OIDCIntrospectionConfig{
				Enabled: introspect, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
				CacheTTL: time.Minute, NegativeCacheTTL: time.Second,
			},
		},
	}
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func (f *fixture) verifier(cfg config.AuthConfig) *Verifier {
	return New(cfg, f.res, tenant.ScopeMetricsQuery)
}

var allMethods = []config.AuthMethod{config.AuthMethodAPIKey, config.AuthMethodJWT, config.AuthMethodOIDC}

func TestAuthenticate_Routing(t *testing.T) {
	f := newFixture(t)
	acme := f.res.AddTenant("acme", "active")
	f.res.AddAPIKey(apiKey, acme)
	f.res.AddAPIKey(opaqueToken, acme) // an API key that doesn't look like one: legacy/foreign format
	f.res.AddMember(f.idp.Issuer(), "alice", acme, "admin")
	f.idp.AddOpaque(opaqueToken, "alice", nil)

	hs256 := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"tenant_id": acme.String(), "exp": time.Now().Add(time.Hour).Unix()})
		s, _ := tok.SignedString([]byte(jwtSecret))
		return s
	}()
	rs256 := f.idp.Sign(t, f.idp.Claims("alice"))

	tests := []struct {
		name       string
		methods    []config.AuthMethod
		introspect bool
		token      string
		wantMethod Method
		wantErr    error
	}{
		{"argv_ prefix is an API key", allMethods, true, apiKey, MethodAPIKey, nil},
		{"argv_ prefix is an API key without introspection", allMethods, false, apiKey, MethodAPIKey, nil},
		{"argv_ prefix when api_key is disabled", []config.AuthMethod{config.AuthMethodOIDC}, true, apiKey, "", ErrUnauthenticated},
		{"HS256 JWS is jwt", allMethods, true, hs256, MethodJWT, nil},
		{"HS256 JWS without the jwt method", []config.AuthMethod{config.AuthMethodOIDC}, true, hs256, "", ErrUnauthenticated},
		{"asymmetric JWS is oidc", allMethods, false, rs256, MethodOIDC, nil},
		{"asymmetric JWS without the oidc method", []config.AuthMethod{config.AuthMethodAPIKey}, false, rs256, "", ErrUnauthenticated},
		{"opaque token with introspection is oidc", allMethods, true, opaqueToken, MethodOIDC, nil},
		{"opaque token, introspection off: falls back to API key (unchanged behaviour)", allMethods, false, opaqueToken, MethodAPIKey, nil},
		{"opaque token, introspection on but oidc method off: API key", []config.AuthMethod{config.AuthMethodAPIKey}, true, opaqueToken, MethodAPIKey, nil},
		{"unknown opaque token with introspection", allMethods, true, "who-knows", "", ErrUnauthenticated},
		{"empty token", allMethods, true, "", "", ErrUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := f.verifier(f.cfg(tt.methods, tt.introspect, nil))
			id, err := v.Authenticate(context.Background(), tt.token)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if id.Method != tt.wantMethod {
				t.Errorf("method = %s, want %s", id.Method, tt.wantMethod)
			}
			if id.Resolved == nil || id.Resolved.Tenant.ID != acme {
				t.Errorf("resolved tenant = %+v, want acme", id.Resolved)
			}
		})
	}
}

func TestAuthenticate_APIKeyNeverReachesIdP(t *testing.T) {
	f := newFixture(t)
	acme := f.res.AddTenant("acme", "active")
	f.res.AddAPIKey(apiKey, acme)
	v := f.verifier(f.cfg(allMethods, true, nil))

	if _, err := v.Authenticate(context.Background(), apiKey); err != nil {
		t.Fatal(err)
	}
	if n := f.idp.IntrospectHits.Load() + f.idp.JWKSHits.Load() + f.idp.DiscoveryHits.Load(); n != 0 {
		t.Errorf("an API key caused %d IdP requests", n)
	}
}

func TestAuthenticate_MembershipMatrix(t *testing.T) {
	f := newFixture(t)
	a := f.res.AddTenant("a", "active")
	b := f.res.AddTenant("b", "active")
	foreign := f.res.AddTenant("foreign", "active")
	f.res.AddMember(f.idp.Issuer(), "one", a, "admin")
	f.res.AddMember(f.idp.Issuer(), "many", a, "viewer")
	f.res.AddMember(f.idp.Issuer(), "many", b, "admin")
	// "none" has no memberships.

	tests := []struct {
		name     string
		sub      string
		hint     string
		wantErr  error
		wantTID  uuid.UUID
		wantRole string
	}{
		{"one membership, no header", "one", "", nil, a, "admin"},
		{"one membership, header for it", "one", a.String(), nil, a, "admin"},
		{"one membership, header for a foreign tenant", "one", foreign.String(), ErrNoTenantAccess, uuid.Nil, ""},
		{"one membership, header for a second tenant they do not belong to", "one", b.String(), ErrNoTenantAccess, uuid.Nil, ""},
		{"many memberships, no header", "many", "", ErrTenantRequired, uuid.Nil, ""},
		{"many memberships, header picks the first", "many", a.String(), nil, a, "viewer"},
		{"many memberships, header picks the second", "many", b.String(), nil, b, "admin"},
		{"many memberships, header for a foreign tenant", "many", foreign.String(), ErrNoTenantAccess, uuid.Nil, ""},
		{"no memberships, no header", "none", "", ErrNoTenantAccess, uuid.Nil, ""},
		{"no memberships, header for an existing tenant", "none", a.String(), ErrNoTenantAccess, uuid.Nil, ""},
		{"header is not a UUID", "one", "acme", ErrNoTenantAccess, uuid.Nil, ""},
		{"header for a tenant that does not exist", "one", uuid.NewString(), ErrNoTenantAccess, uuid.Nil, ""},
	}
	for _, tt := range tests {
		for _, kind := range []string{"jwt", "opaque"} {
			t.Run(tt.name+"/"+kind, func(t *testing.T) {
				v := f.verifier(f.cfg(allMethods, true, nil))
				token := f.idp.Sign(t, f.idp.Claims(tt.sub))
				if kind == "opaque" {
					token = "opaque-" + tt.sub
					f.idp.AddOpaque(token, tt.sub, nil)
				}
				id, err := v.AuthenticateForTenant(context.Background(), token, tt.hint)
				if tt.wantErr != nil {
					if !errors.Is(err, tt.wantErr) {
						t.Fatalf("err = %v, want %v", err, tt.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if id.Resolved.Tenant.ID != tt.wantTID || id.Role != tt.wantRole {
					t.Errorf("tenant/role = %v/%q, want %v/%q", id.Resolved.Tenant.ID, id.Role, tt.wantTID, tt.wantRole)
				}
				if id.Issuer != f.idp.Issuer() || id.Subject != tt.sub {
					t.Errorf("issuer/subject = %q/%q", id.Issuer, id.Subject)
				}
			})
		}
	}
}

func TestAuthenticate_SuspendedTenant(t *testing.T) {
	f := newFixture(t)
	gone := f.res.AddTenant("gone", "suspended")
	f.res.AddMember(f.idp.Issuer(), "alice", gone, "admin")
	v := f.verifier(f.cfg(allMethods, false, nil))

	_, err := v.Authenticate(context.Background(), f.idp.Sign(t, f.idp.Claims("alice")))
	if !errors.Is(err, ErrTenantSuspended) {
		t.Fatalf("err = %v, want ErrTenantSuspended", err)
	}
}

func TestAuthenticate_TenantIDClaim(t *testing.T) {
	f := newFixture(t)
	mine := f.res.AddTenant("mine", "active")
	other := f.res.AddTenant("other", "active")
	f.res.AddMember(f.idp.Issuer(), "alice", mine, "admin")

	token := func(sub string, tid any) string {
		c := f.idp.Claims(sub)
		if tid != nil {
			c["tenant_id"] = tid
		}
		return f.idp.Sign(t, c)
	}

	t.Run("a claim naming a membership is accepted", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, false, nil))
		id, err := v.Authenticate(context.Background(), token("alice", mine.String()))
		if err != nil || id.Resolved.Tenant.ID != mine {
			t.Fatalf("id=%+v err=%v", id, err)
		}
	})
	t.Run("a claim naming a tenant the user does not belong to is refused", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, false, nil))
		if _, err := v.Authenticate(context.Background(), token("alice", other.String())); !errors.Is(err, ErrNoTenantAccess) {
			t.Fatalf("err = %v, want ErrNoTenantAccess", err)
		}
	})
	t.Run("a malformed claim is refused", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, false, nil))
		for _, bad := range []any{"not-a-uuid", 42} {
			if _, err := v.Authenticate(context.Background(), token("alice", bad)); !errors.Is(err, ErrNoTenantAccess) {
				t.Errorf("claim %v: err = %v, want ErrNoTenantAccess", bad, err)
			}
		}
	})
	t.Run("a claim cannot widen what the header may pick", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, false, nil))
		if _, err := v.AuthenticateForTenant(context.Background(), token("alice", mine.String()), other.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Fatalf("err = %v, want ErrNoTenantAccess", err)
		}
	})
	t.Run("the explicit trust option accepts a claim with no membership", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, false, func(c *config.AuthConfig) { c.OIDC.TrustTenantIDClaim = true }))
		id, err := v.Authenticate(context.Background(), token("stranger", other.String()))
		if err != nil || id.Resolved.Tenant.ID != other {
			t.Fatalf("id=%+v err=%v", id, err)
		}
		// ...but the header still can't point elsewhere.
		if _, err := v.AuthenticateForTenant(context.Background(), token("stranger", other.String()), mine.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Fatalf("hint mismatch: err = %v, want ErrNoTenantAccess", err)
		}
	})
	t.Run("trust option without a claim falls back to memberships", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, false, func(c *config.AuthConfig) { c.OIDC.TrustTenantIDClaim = true }))
		id, err := v.Authenticate(context.Background(), token("alice", nil))
		if err != nil || id.Resolved.Tenant.ID != mine {
			t.Fatalf("id=%+v err=%v", id, err)
		}
	})
}

func TestAuthenticate_APIKeyAndJWTWithTenantHeader(t *testing.T) {
	f := newFixture(t)
	acme := f.res.AddTenant("acme", "active")
	other := f.res.AddTenant("other", "active")
	f.res.AddAPIKey(apiKey, acme)
	v := f.verifier(f.cfg(allMethods, false, nil))

	hs256 := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"tenant_id": acme.String(), "exp": time.Now().Add(time.Hour).Unix()})
		s, _ := tok.SignedString([]byte(jwtSecret))
		return s
	}()
	for name, token := range map[string]string{"api key": apiKey, "legacy jwt": hs256} {
		t.Run(name, func(t *testing.T) {
			if _, err := v.AuthenticateForTenant(context.Background(), token, acme.String()); err != nil {
				t.Errorf("own tenant: %v", err)
			}
			if _, err := v.AuthenticateForTenant(context.Background(), token, other.String()); !errors.Is(err, ErrNoTenantAccess) {
				t.Errorf("foreign tenant: err = %v, want ErrNoTenantAccess", err)
			}
			if _, err := v.AuthenticateForTenant(context.Background(), token, ""); err != nil {
				t.Errorf("no header: %v", err)
			}
		})
	}
}

func TestAuthenticate_ErrorClasses(t *testing.T) {
	f := newFixture(t)
	acme := f.res.AddTenant("acme", "active")
	f.res.AddMember(f.idp.Issuer(), "alice", acme, "admin")

	t.Run("missing scope", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, true, func(c *config.AuthConfig) { c.OIDC.RequiredScopes = []string{"admin:all"} }))
		for name, token := range map[string]string{"jwt": f.idp.Sign(t, f.idp.Claims("alice")), "opaque": func() string {
			f.idp.AddOpaque("scope-tok", "alice", nil)
			return "scope-tok"
		}()} {
			_, err := v.Authenticate(context.Background(), token)
			if !errors.Is(err, ErrInsufficientScope) || errors.Is(err, ErrUnauthenticated) {
				t.Errorf("%s: err = %v, want ErrInsufficientScope", name, err)
			}
		}
	})
	t.Run("IdP down is never unauthenticated", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, true, nil))
		f.idp.IntrospectStatus.Store(500)
		defer f.idp.IntrospectStatus.Store(0)
		f.idp.AddOpaque("down-tok", "alice", nil)
		_, err := v.Authenticate(context.Background(), "down-tok")
		if !errors.Is(err, ErrProviderUnavailable) || errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrProviderUnavailable only", err)
		}
	})
	t.Run("bad token is unauthenticated", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, true, nil))
		c := f.idp.Claims("alice")
		c["aud"] = "spa-client-id" // an ID token's audience
		_, err := v.Authenticate(context.Background(), f.idp.Sign(t, c))
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestAuthenticateSubject_NoTenantChosen(t *testing.T) {
	f := newFixture(t)
	a := f.res.AddTenant("a", "active")
	b := f.res.AddTenant("b", "active")
	f.res.AddMember(f.idp.Issuer(), "many", a, "viewer")
	f.res.AddMember(f.idp.Issuer(), "many", b, "viewer")
	v := f.verifier(f.cfg(allMethods, false, nil))

	id, err := v.AuthenticateSubject(context.Background(), f.idp.Sign(t, f.idp.Claims("many")))
	if err != nil {
		t.Fatal(err)
	}
	if id.Resolved != nil || id.Subject != "many" {
		t.Errorf("id = %+v", id)
	}
	ms, err := v.Memberships(context.Background(), id)
	if err != nil || len(ms) != 2 {
		t.Fatalf("memberships = %v, err = %v", ms, err)
	}
}

func TestCheckStartup(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(f.cfg(allMethods, true, nil))
	if err := v.CheckStartup(context.Background()); err != nil {
		t.Fatalf("CheckStartup: %v", err)
	}
	if err := f.verifier(f.cfg([]config.AuthMethod{config.AuthMethodAPIKey}, false, nil)).CheckStartup(context.Background()); err != nil {
		t.Fatalf("no oidc: %v", err)
	}
}
