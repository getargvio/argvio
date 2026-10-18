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
)

const (
	adminAudience = "https://admin.example.com"
	adminScope    = "argvio:admin"
)

// isRoot recognises the subject "root", whichever IdP vouched for it.
var isRoot = AdminRecognizerFunc(func(id Identity) (string, bool) { return "subject-root", id.Subject == "root" })

type adminFixture struct {
	*fixture
	acme, beta, suspended uuid.UUID
}

func newAdminFixture(t *testing.T) *adminFixture {
	f := &adminFixture{fixture: newFixture(t)}
	f.acme = f.res.AddTenant("acme", "active")
	f.beta = f.res.AddTenant("beta", "active")
	f.suspended = f.res.AddTenant("old", "suspended")
	f.res.AddMember(f.idp.Issuer(), "alice", f.acme, "admin")
	return f
}

func (f *adminFixture) adminVerifier(t *testing.T, introspect bool, mutate func(*config.AuthConfig)) *Verifier {
	t.Helper()
	cfg := f.cfg(allMethods, introspect, mutate)
	v := f.verifier(cfg)
	v.EnableAdmin(cfg, isRoot)
	return v
}

// adminToken is a token for sub aimed at the admin audience.
func adminToken(t *testing.T, idp *oidctest.Provider, sub string, mutate func(jwt.MapClaims)) string {
	t.Helper()
	c := idp.Claims(sub)
	c["aud"] = adminAudience
	c["scope"] = "openid " + adminScope
	if mutate != nil {
		mutate(c)
	}
	return idp.Sign(t, c)
}

func TestAdmin_SameVerificationAsUsers(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	v := f.adminVerifier(t, false, nil)
	rootTok := f.idp.Sign(t, f.idp.Claims("root"))

	t.Run("a recognised admin reaches any active tenant without a membership", func(t *testing.T) {
		for _, tid := range []uuid.UUID{f.acme, f.beta} {
			id, err := v.AuthenticateForTenant(ctx, rootTok, tid.String())
			if err != nil {
				t.Fatalf("tenant %s: %v", tid, err)
			}
			if !id.Admin || id.AdminRule != "subject-root" || id.Method != MethodOIDC {
				t.Errorf("identity = %+v, want an OIDC admin matched by subject-root", id)
			}
			if id.Resolved.Tenant.ID != tid || id.Role != tenant.DefaultMemberRole {
				t.Errorf("tenant = %s role = %q", id.Resolved.Tenant.ID, id.Role)
			}
			if id.Issuer != f.idp.Issuer() || id.Subject != "root" {
				t.Errorf("iss/sub = %q/%q", id.Issuer, id.Subject)
			}
		}
	})

	t.Run("a non-admin is unaffected and still needs a membership", func(t *testing.T) {
		id, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("alice")), "")
		if err != nil || id.Admin || id.Resolved.Tenant.ID != f.acme || id.Role != "admin" {
			t.Fatalf("alice: id=%+v err=%v", id, err)
		}
		if _, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("alice")), f.beta.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("alice on beta: err = %v, want ErrNoTenantAccess", err)
		}
		if _, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("mallory")), f.acme.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("mallory: err = %v, want ErrNoTenantAccess", err)
		}
	})

	t.Run("unknown, inactive and malformed tenants are no_tenant_access", func(t *testing.T) {
		for name, hint := range map[string]string{
			"unknown":   uuid.NewString(),
			"suspended": f.suspended.String(),
			"malformed": "not-a-uuid",
			"a slug":    "acme",
		} {
			if _, err := v.AuthenticateForTenant(ctx, rootTok, hint); !errors.Is(err, ErrNoTenantAccess) {
				t.Errorf("%s: err = %v, want ErrNoTenantAccess", name, err)
			}
		}
	})

	t.Run("no hint: several tenants need a choice", func(t *testing.T) {
		if _, err := v.AuthenticateForTenant(ctx, rootTok, ""); !errors.Is(err, ErrTenantRequired) {
			t.Errorf("err = %v, want ErrTenantRequired", err)
		}
	})

	t.Run("no hint: a lone active tenant is chosen, none is no access", func(t *testing.T) {
		one := newFixture(t)
		only := one.res.AddTenant("only", "active")
		cfg := one.cfg(allMethods, false, nil)
		v := one.verifier(cfg)
		v.EnableAdmin(cfg, isRoot)
		tok := one.idp.Sign(t, one.idp.Claims("root"))
		if id, err := v.AuthenticateForTenant(ctx, tok, ""); err != nil || id.Resolved.Tenant.ID != only {
			t.Fatalf("one tenant: id=%+v err=%v", id, err)
		}

		none := newFixture(t)
		none.res.AddTenant("old", "suspended")
		cfg = none.cfg(allMethods, false, nil)
		v = none.verifier(cfg)
		v.EnableAdmin(cfg, isRoot)
		if _, err := v.AuthenticateForTenant(ctx, none.idp.Sign(t, none.idp.Claims("root")), ""); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("no tenants: err = %v, want ErrNoTenantAccess", err)
		}
	})

	t.Run("AuthenticateSubject marks the admin without choosing a tenant", func(t *testing.T) {
		id, err := v.AuthenticateSubject(ctx, rootTok)
		if err != nil || !id.Admin || id.Resolved != nil {
			t.Fatalf("id=%+v err=%v", id, err)
		}
	})

	t.Run("Memberships is every active tenant, as viewer", func(t *testing.T) {
		id, _ := v.AuthenticateSubject(ctx, rootTok)
		ms, err := v.Memberships(ctx, id)
		if err != nil || len(ms) != 2 || ms[0].Tenant.ID != f.acme || ms[1].Tenant.ID != f.beta {
			t.Fatalf("memberships = %+v, err %v", ms, err)
		}
		for _, m := range ms {
			if m.Role != tenant.DefaultMemberRole {
				t.Errorf("role = %q", m.Role)
			}
		}
	})

	t.Run("a tenant created later is visible at once", func(t *testing.T) {
		gamma := f.res.AddTenant("gamma", "active")
		if _, err := v.AuthenticateForTenant(ctx, rootTok, gamma.String()); err != nil {
			t.Fatal(err)
		}
		id, _ := v.AuthenticateSubject(ctx, rootTok)
		if ms, _ := v.Memberships(ctx, id); len(ms) != 3 {
			t.Errorf("memberships = %d, want 3", len(ms))
		}
	})

	t.Run("one verification serves both: a single JWKS fetch", func(t *testing.T) {
		g := newAdminFixture(t)
		v := g.adminVerifier(t, false, nil)
		for range 5 {
			if _, err := v.AuthenticateSubject(ctx, g.idp.Sign(t, g.idp.Claims("root"))); err != nil {
				t.Fatal(err)
			}
		}
		if n := g.idp.JWKSHits.Load(); n != 1 {
			t.Errorf("JWKS fetched %d times, want 1", n)
		}
	})
}

func TestAdmin_OffByDefault(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	rootTok := f.idp.Sign(t, f.idp.Claims("root"))

	t.Run("no EnableAdmin", func(t *testing.T) {
		v := f.verifier(f.cfg(allMethods, false, nil))
		if _, err := v.AuthenticateForTenant(ctx, rootTok, f.acme.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Fatalf("err = %v, want ErrNoTenantAccess", err)
		}
	})
	t.Run("EnableAdmin with no recognition rule", func(t *testing.T) {
		cfg := f.cfg(allMethods, false, nil)
		v := f.verifier(cfg)
		v.EnableAdmin(cfg)
		id, err := v.AuthenticateSubject(ctx, rootTok)
		if err != nil || id.Admin {
			t.Fatalf("id=%+v err=%v", id, err)
		}
	})
	t.Run("EnableAdmin without the oidc method", func(t *testing.T) {
		cfg := f.cfg([]config.AuthMethod{config.AuthMethodAPIKey}, false, nil)
		v := f.verifier(cfg)
		v.EnableAdmin(cfg, isRoot)
		if v.admin != nil {
			t.Fatal("admin enabled without oidc")
		}
	})
	t.Run("API keys and legacy JWTs are never admins", func(t *testing.T) {
		f.res.AddAPIKey(apiKey, f.acme)
		v := f.adminVerifier(t, false, nil)
		id, err := v.Authenticate(ctx, apiKey)
		if err != nil || id.Admin {
			t.Fatalf("api key: id=%+v err=%v", id, err)
		}
		hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"tenant_id": f.acme.String(), "sub": "root", "exp": 4102444800})
		s, _ := hs.SignedString([]byte(jwtSecret))
		id, err = v.Authenticate(ctx, s)
		if err != nil || id.Admin || id.Method != MethodJWT {
			t.Fatalf("jwt: id=%+v err=%v", id, err)
		}
	})
}

func TestAdmin_DistinctAudienceAndScopes(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	v := f.adminVerifier(t, false, func(c *config.AuthConfig) {
		c.OIDC.RequiredScopes = []string{"metrics:read"}
		c.Admin.OIDC = config.AdminOIDCConfig{Audience: adminAudience, RequiredScopes: []string{adminScope}}
	})

	t.Run("a token for the admin audience, recognised, is an admin", func(t *testing.T) {
		id, err := v.AuthenticateForTenant(ctx, adminToken(t, f.idp, "root", nil), f.beta.String())
		if err != nil || !id.Admin || id.Resolved.Tenant.ID != f.beta {
			t.Fatalf("id=%+v err=%v", id, err)
		}
	})

	t.Run("a user-audience token from an admin subject is not an admin", func(t *testing.T) {
		id, err := v.AuthenticateSubject(ctx, f.idp.Sign(t, f.idp.Claims("root")))
		if err != nil || id.Admin {
			t.Fatalf("id=%+v err=%v", id, err)
		}
		if _, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("root")), f.acme.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("err = %v, want ErrNoTenantAccess", err)
		}
	})

	t.Run("an admin-audience token from someone unrecognised is not accepted as a user", func(t *testing.T) {
		if _, err := v.AuthenticateSubject(ctx, adminToken(t, f.idp, "alice", nil)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})

	t.Run("wrong audience for both", func(t *testing.T) {
		tok := adminToken(t, f.idp, "root", func(c jwt.MapClaims) { c["aud"] = "https://elsewhere.example.com" })
		if _, err := v.AuthenticateSubject(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})

	t.Run("wrong issuer", func(t *testing.T) {
		tok := adminToken(t, f.idp, "root", func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" })
		if _, err := v.AuthenticateSubject(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})

	t.Run("a missing admin scope is insufficient_scope, not a 401", func(t *testing.T) {
		tok := adminToken(t, f.idp, "root", func(c jwt.MapClaims) { c["scope"] = "openid" })
		if _, err := v.AuthenticateSubject(ctx, tok); !errors.Is(err, ErrInsufficientScope) {
			t.Fatalf("err = %v, want ErrInsufficientScope", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		tok := adminToken(t, f.idp, "root", func(c jwt.MapClaims) { c["exp"] = 1 })
		if _, err := v.AuthenticateSubject(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestAdmin_DifferentIssuer(t *testing.T) {
	f := newAdminFixture(t)
	other := oidctest.New(t)
	ctx := context.Background()
	f.res.AddMember(f.idp.Issuer(), "root", f.acme, "viewer") // a user-IdP membership must not matter to an admin
	v := f.adminVerifier(t, false, func(c *config.AuthConfig) {
		c.Admin.OIDC.IssuerURL = other.Issuer()
	})

	t.Run("a token from the admin issuer, recognised, is an admin of that issuer", func(t *testing.T) {
		id, err := v.AuthenticateForTenant(ctx, other.Sign(t, other.Claims("root")), f.beta.String())
		if err != nil || !id.Admin || id.Issuer != other.Issuer() {
			t.Fatalf("id=%+v err=%v", id, err)
		}
	})

	t.Run("the same subject from the user issuer is not an admin", func(t *testing.T) {
		id, err := v.AuthenticateSubject(ctx, f.idp.Sign(t, f.idp.Claims("root")))
		if err != nil || id.Admin {
			t.Fatalf("id=%+v err=%v", id, err)
		}
		if _, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("root")), f.beta.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("err = %v, want ErrNoTenantAccess (member of acme only)", err)
		}
	})

	t.Run("each verifier has its own JWKS cache", func(t *testing.T) {
		for range 3 {
			_, _ = v.AuthenticateSubject(ctx, other.Sign(t, other.Claims("root")))
		}
		if n := other.JWKSHits.Load(); n != 1 {
			t.Errorf("admin IdP JWKS fetched %d times, want 1", n)
		}
	})

	t.Run("the user issuer's credentials do not follow users to another issuer", func(t *testing.T) {
		cfg := f.cfg(allMethods, true, func(c *config.AuthConfig) { c.Admin.OIDC.IssuerURL = other.Issuer() })
		eff := cfg.Admin.OIDC.Effective(cfg.OIDC)
		if eff.Introspection.Enabled || eff.Introspection.ClientSecret != "" || eff.Introspection.ClientID != "" {
			t.Errorf("introspection leaked to another issuer: %+v", eff.Introspection)
		}
	})

	t.Run("an admin IdP outage is a 503 for its tokens but leaves user tokens alone", func(t *testing.T) {
		down := oidctest.New(t)
		down.JWKSStatus.Store(500)
		cfg := f.cfg(allMethods, false, func(c *config.AuthConfig) { c.Admin.OIDC.IssuerURL = down.Issuer() })
		v := f.verifier(cfg)
		v.EnableAdmin(cfg, isRoot)

		if _, err := v.AuthenticateSubject(ctx, down.Sign(t, down.Claims("root"))); !errors.Is(err, ErrProviderUnavailable) {
			t.Errorf("admin token: err = %v, want ErrProviderUnavailable", err)
		}
		id, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("alice")), "")
		if err != nil || id.Admin || id.Resolved.Tenant.ID != f.acme {
			t.Errorf("user token: id=%+v err=%v", id, err)
		}
	})
}

func TestAdmin_OpaqueTokens(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	const adminOpaque, userOpaque = "opaque-root", "opaque-alice"
	f.idp.AddOpaque(adminOpaque, "root", nil)
	f.idp.AddOpaque(userOpaque, "alice", nil)

	t.Run("same verification: introspected once, cached", func(t *testing.T) {
		v := f.adminVerifier(t, true, nil)
		for range 4 {
			id, err := v.AuthenticateForTenant(ctx, adminOpaque, f.beta.String())
			if err != nil || !id.Admin || id.Resolved.Tenant.ID != f.beta {
				t.Fatalf("id=%+v err=%v", id, err)
			}
		}
		if n := f.idp.IntrospectHits.Load(); n != 1 {
			t.Errorf("introspected %d times, want 1", n)
		}
		if id, err := v.AuthenticateForTenant(ctx, userOpaque, ""); err != nil || id.Admin {
			t.Errorf("user opaque: id=%+v err=%v", id, err)
		}
	})

	t.Run("only admin verification introspects", func(t *testing.T) {
		on := true
		f.res.AddAPIKey("legacy-key", f.acme) // an API key that doesn't look like one
		v := f.adminVerifier(t, false, func(c *config.AuthConfig) {
			c.Admin.OIDC.Introspection = config.AdminIntrospectionConfig{
				Enabled: &on, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
				CacheTTL: time.Minute, NegativeCacheTTL: time.Second,
			}
		})
		if id, err := v.AuthenticateForTenant(ctx, adminOpaque, f.acme.String()); err != nil || !id.Admin {
			t.Fatalf("admin opaque: id=%+v err=%v", id, err)
		}
		// Not an admin, and users do not introspect: unchanged old behaviour.
		if _, err := v.Authenticate(ctx, userOpaque); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("user opaque: err = %v, want ErrUnauthenticated", err)
		}
		if id, err := v.Authenticate(ctx, "legacy-key"); err != nil || id.Method != MethodAPIKey {
			t.Errorf("legacy key: id=%+v err=%v", id, err)
		}
	})
}

func TestAdmin_CheckStartupCoversAdminVerifier(t *testing.T) {
	f := newAdminFixture(t)
	other := oidctest.New(t)
	on := true
	cfg := f.cfg(allMethods, false, func(c *config.AuthConfig) {
		c.Admin.OIDC.IssuerURL = other.Issuer()
		c.Admin.OIDC.Introspection = config.AdminIntrospectionConfig{
			Enabled: &on, Endpoint: "", ClientID: "x", ClientSecret: "y", CacheTTL: time.Minute, NegativeCacheTTL: time.Second,
		}
	})
	v := f.verifier(cfg)
	v.EnableAdmin(cfg, isRoot)
	if err := v.CheckStartup(context.Background()); err != nil {
		t.Fatalf("CheckStartup: %v", err) // endpoint comes from the admin IdP's discovery document
	}
	if n := other.DiscoveryHits.Load(); n == 0 {
		t.Error("the admin IdP's discovery document was not checked at startup")
	}
}
