package otlp

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/oidc/oidctest"
	"github.com/getargvio/argvio/internal/ratelimit"
	"github.com/getargvio/argvio/internal/tenant"
	"github.com/getargvio/argvio/internal/tenant/tenanttest"
)

// TestCoreAuthenticate_AuthMethods checks that the public server's auth stage
// still takes API keys exactly as before, and maps the new OIDC failure classes
// to the right ingest statuses.
func TestCoreAuthenticate_AuthMethods(t *testing.T) {
	idp := oidctest.New(t)
	res := tenanttest.New()
	acme := res.AddTenant("acme", "active")
	const key = "argv_live_pub_abcdef"
	res.AddAPIKey(key, acme)
	res.AddMember(idp.Issuer(), "solo", acme, "viewer")

	oidcCfg := config.OIDCConfig{IssuerURL: idp.Issuer(), Audience: oidctest.Audience, JWKSCacheTTL: time.Hour}
	newCore := func(methods ...config.AuthMethod) *core {
		return &core{
			verifier: authn.New(config.AuthConfig{Methods: methods, OIDC: oidcCfg}, res, tenant.ScopePublicIngest),
			limiter:  ratelimit.New(ratelimit.Defaults{RequestsPerSecond: 1000, BytesPerSecond: 1e9, Burst: 1000}, nil),
		}
	}

	t.Run("API key ingest is unchanged, with or without oidc enabled", func(t *testing.T) {
		for _, c := range []*core{newCore(config.AuthMethodAPIKey), newCore(config.AuthMethodAPIKey, config.AuthMethodOIDC)} {
			id, err := c.authenticate(context.Background(), key, 10)
			if err != nil || id.Method != authn.MethodAPIKey || id.Resolved.Tenant.ID != acme {
				t.Fatalf("id=%+v err=%v", id, err)
			}
		}
	})

	t.Run("an OIDC token is not accepted unless oidc is enabled", func(t *testing.T) {
		_, err := newCore(config.AuthMethodAPIKey).authenticate(context.Background(), idp.Sign(t, idp.Claims("solo")), 10)
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})

	t.Run("an OIDC identity with one membership can ingest", func(t *testing.T) {
		id, err := newCore(config.AuthMethodOIDC).authenticate(context.Background(), idp.Sign(t, idp.Claims("solo")), 10)
		if err != nil || id.Resolved.Tenant.ID != acme {
			t.Fatalf("id=%+v err=%v", id, err)
		}
	})

	t.Run("no membership is forbidden (403 / PermissionDenied)", func(t *testing.T) {
		_, err := newCore(config.AuthMethodOIDC).authenticate(context.Background(), idp.Sign(t, idp.Claims("stranger")), 10)
		if !errors.Is(err, ErrForbidden) || httpStatusForAuthError(err) != http.StatusForbidden || status.Code(grpcAuthError(err)) != codes.PermissionDenied {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an identity provider outage is retryable (503 / Unavailable)", func(t *testing.T) {
		down := oidctest.New(t)
		down.JWKSStatus.Store(500)
		c := &core{
			verifier: authn.New(config.AuthConfig{
				Methods: []config.AuthMethod{config.AuthMethodOIDC},
				OIDC:    config.OIDCConfig{IssuerURL: down.Issuer(), Audience: oidctest.Audience, JWKSCacheTTL: time.Hour},
			}, res, tenant.ScopePublicIngest),
			limiter: ratelimit.New(ratelimit.Defaults{RequestsPerSecond: 1000, Burst: 1000}, nil),
		}
		_, err := c.authenticate(context.Background(), down.Sign(t, down.Claims("solo")), 10)
		if !errors.Is(err, ErrUnavailable) || httpStatusForAuthError(err) != http.StatusServiceUnavailable || status.Code(grpcAuthError(err)) != codes.Unavailable {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestCoreAuthenticate_NeverAdmin: admin identity is a metrics-server concept.
// The public server builds its verifier without EnableAdmin, so even a token
// an admin rule would accept is an ordinary identity here, and should a
// verifier with admin enabled ever be wired in, ingest refuses its identities
// rather than let them write into a tenant nobody granted them.
func TestCoreAuthenticate_NeverAdmin(t *testing.T) {
	idp := oidctest.New(t)
	res := tenanttest.New()
	acme := res.AddTenant("acme", "active")
	res.AddMember(idp.Issuer(), "member", acme, "viewer")
	// "root" is not a member of anything; only an admin could reach acme.
	cfg := config.AuthConfig{
		Methods: []config.AuthMethod{config.AuthMethodOIDC},
		OIDC:    config.OIDCConfig{IssuerURL: idp.Issuer(), Audience: oidctest.Audience, JWKSCacheTTL: time.Hour},
	}
	newCore := func(v *authn.Verifier) *core {
		return &core{
			verifier: v,
			limiter:  ratelimit.New(ratelimit.Defaults{RequestsPerSecond: 1000, BytesPerSecond: 1e9, Burst: 1000}, nil),
		}
	}
	isRoot := authn.AdminRecognizerFunc(func(id authn.Identity) (string, bool) { return "test", id.Subject == "root" })

	t.Run("a public verifier does not recognise admins", func(t *testing.T) {
		_, err := newCore(authn.New(cfg, res, tenant.ScopePublicIngest)).authenticate(context.Background(), idp.Sign(t, idp.Claims("root")), 10)
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden (no membership)", err)
		}
	})

	t.Run("ingest refuses an admin identity even from a verifier that has admin enabled", func(t *testing.T) {
		v := authn.New(cfg, res, tenant.ScopePublicIngest)
		v.EnableAdmin(cfg, isRoot)
		c := newCore(v)
		if _, err := c.authenticate(context.Background(), idp.Sign(t, idp.Claims("root")), 10); !errors.Is(err, ErrForbidden) {
			t.Fatalf("admin: err = %v, want ErrForbidden", err)
		}
		if id, err := c.authenticate(context.Background(), idp.Sign(t, idp.Claims("member")), 10); err != nil || id.Resolved.Tenant.ID != acme {
			t.Fatalf("member still ingests: id=%+v err=%v", id, err)
		}
	})
}
