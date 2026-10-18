package authn

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/role"
)

func TestClaimRule(t *testing.T) {
	rule := claimRule{name: "groups", value: "argvio-admins"}
	tests := []struct {
		name   string
		claims jwt.MapClaims
		want   bool
	}{
		{"string claim equal", jwt.MapClaims{"groups": "argvio-admins"}, true},
		{"array containing the value", jwt.MapClaims{"groups": []any{"eng", "argvio-admins"}}, true},
		{"[]string containing the value", jwt.MapClaims{"groups": []string{"argvio-admins"}}, true},
		{"string claim different", jwt.MapClaims{"groups": "eng"}, false},
		{"array without the value", jwt.MapClaims{"groups": []any{"eng", "ops"}}, false},
		{"missing claim", jwt.MapClaims{"sub": "x"}, false},
		{"nil claims", nil, false},
		{"case differs", jwt.MapClaims{"groups": "Argvio-Admins"}, false},
		{"substring is not a match", jwt.MapClaims{"groups": "argvio-admins-readonly"}, false},
		{"space-separated string is not split", jwt.MapClaims{"groups": "eng argvio-admins"}, false},
		{"wrong type: number", jwt.MapClaims{"groups": 1.0}, false},
		{"wrong type: bool", jwt.MapClaims{"groups": true}, false},
		{"wrong type: object", jwt.MapClaims{"groups": map[string]any{"argvio-admins": true}}, false},
		{"array of non-strings", jwt.MapClaims{"groups": []any{1.0, true, nil}}, false},
		{"a different claim holds the value", jwt.MapClaims{"roles": "argvio-admins"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, ok := rule.Recognize(Identity{Claims: tt.claims})
			if ok != tt.want || (ok && name != "claim") {
				t.Errorf("Recognize = %q, %v; want match=%v", name, ok, tt.want)
			}
		})
	}
}

func TestSubjectRule(t *testing.T) {
	rule := subjectRule{issuer: "https://idp.example.com", subjects: []string{"alice", "bob"}}
	tests := []struct {
		name string
		id   Identity
		want bool
	}{
		{"listed subject", Identity{Issuer: "https://idp.example.com", Subject: "bob"}, true},
		{"same sub, different issuer", Identity{Issuer: "https://other.example.com", Subject: "alice"}, false},
		{"issuer differs by a trailing slash", Identity{Issuer: "https://idp.example.com/", Subject: "alice"}, false},
		{"unlisted subject", Identity{Issuer: "https://idp.example.com", Subject: "mallory"}, false},
		{"case differs", Identity{Issuer: "https://idp.example.com", Subject: "Alice"}, false},
		{"prefix is not a match", Identity{Issuer: "https://idp.example.com", Subject: "alic"}, false},
		{"no subject", Identity{Issuer: "https://idp.example.com"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, ok := rule.Recognize(tt.id)
			if ok != tt.want || (ok && name != "subjects") {
				t.Errorf("Recognize = %q, %v; want match=%v", name, ok, tt.want)
			}
		})
	}
}

func TestAdminRecognizersFromConfig(t *testing.T) {
	if got := adminRecognizersFromConfig(config.AdminConfig{}, "https://idp"); len(got) != 0 {
		t.Errorf("nothing configured: %d rules", len(got))
	}
	// A half-configured claim never reaches here (it fails validation), but
	// must not recognise anyone if it did.
	if got := adminRecognizersFromConfig(config.AdminConfig{Claim: config.AdminClaimConfig{Name: "groups"}}, "https://idp"); len(got) != 0 {
		t.Errorf("claim name only: %d rules", len(got))
	}
	if got := adminRecognizersFromConfig(config.AdminConfig{Subjects: []string{" ", ""}}, "https://idp"); len(got) != 0 {
		t.Errorf("blank subjects: %d rules", len(got))
	}
	both := config.AdminConfig{Claim: config.AdminClaimConfig{Name: "groups", Value: "g"}, Subjects: []string{"alice"}}
	if got := adminRecognizersFromConfig(both, "https://idp"); len(got) != 2 {
		t.Errorf("both configured: %d rules, want 2", len(got))
	}
}

func TestAdmin_ConfiguredRules(t *testing.T) {
	ctx := context.Background()
	f := newAdminFixture(t)
	withAdmin := func(a config.AdminConfig) func(*config.AuthConfig) {
		return func(c *config.AuthConfig) { c.Admin = a }
	}
	enable := func(t *testing.T, introspect bool, a config.AdminConfig) *Verifier {
		t.Helper()
		cfg := f.cfg(allMethods, introspect, withAdmin(a))
		v := f.verifier(cfg)
		v.EnableAdmin(cfg)
		return v
	}
	claimCfg := config.AdminConfig{Claim: config.AdminClaimConfig{Name: "groups", Value: "argvio-admins"}}

	t.Run("claim in a JWT", func(t *testing.T) {
		v := enable(t, false, claimCfg)
		c := f.idp.Claims("carol")
		c["groups"] = []string{"eng", "argvio-admins"}
		id, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, c), f.acme.String())
		if err != nil || !id.Admin || id.AdminRule != "claim" || id.Resolved.Tenant.ID != f.acme {
			t.Fatalf("id=%+v err=%v", id, err)
		}

		c = f.idp.Claims("carol")
		c["groups"] = []string{"eng"}
		if _, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, c), f.acme.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("without the group: err = %v, want ErrNoTenantAccess", err)
		}
	})

	t.Run("claim in an introspection response", func(t *testing.T) {
		v := enable(t, true, claimCfg)
		f.idp.AddOpaque("opaque-admin", "carol", func(m map[string]any) { m["groups"] = []any{"argvio-admins"} })
		f.idp.AddOpaque("opaque-user", "carol", func(m map[string]any) { m["groups"] = []any{"eng"} })

		id, err := v.AuthenticateForTenant(ctx, "opaque-admin", f.beta.String())
		if err != nil || !id.Admin || id.AdminRule != "claim" {
			t.Fatalf("admin: id=%+v err=%v", id, err)
		}
		if _, err := v.AuthenticateForTenant(ctx, "opaque-user", f.beta.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("user: err = %v, want ErrNoTenantAccess", err)
		}
	})

	t.Run("a token that fails verification is no admin, whatever it claims", func(t *testing.T) {
		// Only a verified token's claims reach a rule (here: wrong audience).
		v := enable(t, false, claimCfg)
		c := f.idp.Claims("carol")
		c["aud"] = "https://someone-elses-api"
		c["groups"] = []string{"argvio-admins"}
		if _, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, c), f.acme.String()); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("err = %v, want ErrUnauthenticated", err)
		}
	})

	subjectCfg := func(subs ...string) config.AdminConfig { return config.AdminConfig{Subjects: subs} }

	t.Run("subject allowlist matches the verified iss and sub", func(t *testing.T) {
		v := enable(t, false, subjectCfg(" dave ", "", "dave", "erin"))
		for _, sub := range []string{"dave", "erin"} {
			id, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims(sub)), f.acme.String())
			if err != nil || !id.Admin || id.AdminRule != "subjects" {
				t.Errorf("%s: id=%+v err=%v", sub, id, err)
			}
		}
		if _, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("mallory")), f.acme.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("mallory: err = %v, want ErrNoTenantAccess", err)
		}
	})

	t.Run("subject allowlist against an opaque token", func(t *testing.T) {
		v := enable(t, true, subjectCfg("dave"))
		f.idp.AddOpaque("opaque-dave", "dave", nil)
		if id, err := v.AuthenticateForTenant(ctx, "opaque-dave", f.acme.String()); err != nil || !id.Admin {
			t.Fatalf("id=%+v err=%v", id, err)
		}
	})

	t.Run("subjects follow the admin issuer, not the user issuer", func(t *testing.T) {
		other := newFixture(t)
		cfg := f.cfg(allMethods, false, func(c *config.AuthConfig) {
			c.Admin = subjectCfg("dave")
			c.Admin.OIDC.IssuerURL = other.idp.Issuer()
			c.Admin.OIDC.Audience = adminAudience
		})
		v := f.verifier(cfg)
		v.EnableAdmin(cfg)

		// "dave" at the user IdP is not the allowlisted admin.
		if _, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("dave")), f.acme.String()); !errors.Is(err, ErrNoTenantAccess) {
			t.Errorf("user-IdP dave: err = %v, want ErrNoTenantAccess", err)
		}
		c := other.idp.Claims("dave")
		c["aud"] = adminAudience
		id, err := v.AuthenticateForTenant(ctx, other.idp.Sign(t, c), f.acme.String())
		if err != nil || !id.Admin || id.Issuer != other.idp.Issuer() {
			t.Errorf("admin-IdP dave: id=%+v err=%v", id, err)
		}
	})

	t.Run("either rule suffices", func(t *testing.T) {
		both := claimCfg
		both.Subjects = []string{"dave"}
		v := enable(t, false, both)
		if id, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, f.idp.Claims("dave")), f.acme.String()); err != nil || id.AdminRule != "subjects" {
			t.Errorf("dave: id=%+v err=%v", id, err)
		}
		c := f.idp.Claims("carol")
		c["groups"] = "argvio-admins"
		if id, err := v.AuthenticateForTenant(ctx, f.idp.Sign(t, c), f.acme.String()); err != nil || id.AdminRule != "claim" {
			t.Errorf("carol: id=%+v err=%v", id, err)
		}
	})
}

func TestAdmin_Role(t *testing.T) {
	ctx := context.Background()
	f := newAdminFixture(t)
	token := func(t *testing.T, mutate func(jwt.MapClaims)) string {
		c := f.idp.Claims("dave")
		if mutate != nil {
			mutate(c)
		}
		return f.idp.Sign(t, c)
	}
	setup := func(t *testing.T, a config.AdminConfig) (*Verifier, *bytes.Buffer) {
		t.Helper()
		a.Subjects = []string{"dave"}
		cfg := f.cfg(allMethods, false, func(c *config.AuthConfig) { c.Admin = a })
		v := f.verifier(cfg)
		v.EnableAdmin(cfg)
		var logs bytes.Buffer
		v.admin.log = slog.New(slog.NewTextHandler(&logs, nil))
		return v, &logs
	}
	roleOf := func(t *testing.T, v *Verifier, tok string) string {
		t.Helper()
		id, err := v.AuthenticateForTenant(ctx, tok, f.acme.String())
		if err != nil || !id.Admin {
			t.Fatalf("id=%+v err=%v", id, err)
		}
		ms, err := v.Memberships(ctx, id)
		if err != nil || len(ms) == 0 {
			t.Fatalf("memberships=%v err=%v", ms, err)
		}
		for _, m := range ms {
			if m.Role != id.Role {
				t.Errorf("membership role %q differs from identity role %q", m.Role, id.Role)
			}
		}
		return id.Role
	}

	t.Run("default is viewer", func(t *testing.T) {
		v, _ := setup(t, config.AdminConfig{})
		if got := roleOf(t, v, token(t, nil)); got != role.Viewer {
			t.Errorf("role = %q", got)
		}
	})

	t.Run("fixed role", func(t *testing.T) {
		v, _ := setup(t, config.AdminConfig{Role: role.Admin})
		if got := roleOf(t, v, token(t, nil)); got != role.Admin {
			t.Errorf("role = %q", got)
		}
	})

	t.Run("role claim wins over the configured role", func(t *testing.T) {
		v, logs := setup(t, config.AdminConfig{RoleClaim: "argvio_role"})
		if got := roleOf(t, v, token(t, func(c jwt.MapClaims) { c["argvio_role"] = role.Admin })); got != role.Admin {
			t.Errorf("role = %q", got)
		}
		v2, _ := setup(t, config.AdminConfig{Role: role.Admin, RoleClaim: "argvio_role"})
		if got := roleOf(t, v2, token(t, func(c jwt.MapClaims) { c["argvio_role"] = role.Viewer })); got != role.Viewer {
			t.Errorf("a claim may also lower the role: %q", got)
		}
		if logs.Len() != 0 {
			t.Errorf("a valid claim logged: %s", logs)
		}
	})

	t.Run("a missing claim falls back to the configured role", func(t *testing.T) {
		v, logs := setup(t, config.AdminConfig{Role: role.Viewer, RoleClaim: "argvio_role"})
		if got := roleOf(t, v, token(t, nil)); got != role.Viewer {
			t.Errorf("role = %q", got)
		}
		if logs.Len() != 0 {
			t.Errorf("an absent claim is not worth a log line: %s", logs)
		}
	})

	t.Run("an invalid claim falls back to the configured role and is logged", func(t *testing.T) {
		for name, val := range map[string]any{
			"unknown name": "superuser",
			"wrong case":   "Admin",
			"wrong type":   []any{"admin"},
			"number":       1.0,
		} {
			v, logs := setup(t, config.AdminConfig{Role: role.Viewer, RoleClaim: "argvio_role"})
			if got := roleOf(t, v, token(t, func(c jwt.MapClaims) { c["argvio_role"] = val })); got != role.Viewer {
				t.Errorf("%s: role = %q, want the configured viewer", name, got)
			}
			if out := logs.String(); !strings.Contains(out, "argvio_role") || !strings.Contains(out, "sub=dave") {
				t.Errorf("%s: log = %q", name, out)
			}
		}
	})

	t.Run("a role claim from the introspection response", func(t *testing.T) {
		cfg := f.cfg(allMethods, true, func(c *config.AuthConfig) {
			c.Admin = config.AdminConfig{Subjects: []string{"dave"}, RoleClaim: "argvio_role"}
		})
		v := f.verifier(cfg)
		v.EnableAdmin(cfg)
		f.idp.AddOpaque("opaque-dave-admin", "dave", func(m map[string]any) { m["argvio_role"] = role.Admin })
		if got := roleOf(t, v, "opaque-dave-admin"); got != role.Admin {
			t.Errorf("role = %q", got)
		}
	})
}
