package metricsapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/config"
)

var isRoot = authn.AdminRecognizerFunc(func(id authn.Identity) (string, bool) { return "subject-root", id.Subject == "root" })

func (f *authFixture) adminServer(t *testing.T) *Server {
	t.Helper()
	cfg := f.authConfig(false, nil)
	srv := f.server(cfg, config.CORSConfig{})
	srv.Verifier.EnableAdmin(cfg, isRoot)
	return srv
}

func TestAdmin_TenantSelection(t *testing.T) {
	f := newAuthFixture(t)
	a := f.res.AddTenant("Acme", "active")
	b := f.res.AddTenant("Beta", "active")
	old := f.res.AddTenant("Old", "suspended")
	f.res.AddMember(f.idp.Issuer(), "alice", a, "admin")
	srv := f.adminServer(t)

	tests := []struct {
		name       string
		sub        string
		header     string
		wantStatus int
		wantCode   string
		wantTenant uuid.UUID
	}{
		{"admin selects any active tenant", "root", b.String(), 200, "", b},
		{"admin selects another one", "root", a.String(), 200, "", a},
		{"admin with several tenants must choose", "root", "", 400, "tenant_required", uuid.Nil},
		{"admin and an unknown tenant", "root", uuid.NewString(), 403, "no_tenant_access", uuid.Nil},
		{"admin and a suspended tenant", "root", old.String(), 403, "no_tenant_access", uuid.Nil},
		{"admin and a malformed tenant", "root", "acme", 403, "no_tenant_access", uuid.Nil},
		{"non-admin keeps to memberships", "alice", b.String(), 403, "no_tenant_access", uuid.Nil},
		{"non-admin member", "alice", "", 200, "", a},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var res probeResult
			hdr := map[string]string{}
			if tt.header != "" {
				hdr[TenantHeader] = tt.header
			}
			rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", f.idp.Sign(t, f.idp.Claims(tt.sub)), hdr)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" && errCode(t, rec) != tt.wantCode {
				t.Errorf("error code = %q, want %q", errCode(t, rec), tt.wantCode)
			}
			if res.tenant != tt.wantTenant {
				t.Errorf("scoped tenant = %v, want %v", res.tenant, tt.wantTenant)
			}
		})
	}

	t.Run("the scope role is least-privileged", func(t *testing.T) {
		var res probeResult
		do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", f.idp.Sign(t, f.idp.Claims("root")), map[string]string{TenantHeader: a.String()})
		if res.role != "viewer" {
			t.Errorf("role = %q, want viewer", res.role)
		}
	})
}

func TestAdmin_AuditLog(t *testing.T) {
	f := newAuthFixture(t)
	a := f.res.AddTenant("Acme", "active")
	srv := f.adminServer(t)
	tok := f.idp.Sign(t, f.idp.Claims("root"))

	var res probeResult
	if rec := do(srv.authMiddleware(probe(&res)), "GET", "/v1/traces", tok, map[string]string{TenantHeader: a.String()}); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	logs := f.logs.String()
	line := ""
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "admin selected tenant") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no audit line in:\n%s", logs)
	}
	for _, want := range []string{"iss=" + f.idp.Issuer(), "sub=root", "tenant_id=" + a.String(), "rule=subject-root"} {
		if !strings.Contains(line, want) {
			t.Errorf("audit line %q lacks %q", line, want)
		}
	}
	if strings.Contains(logs, tok) {
		t.Error("the token was logged")
	}

	t.Run("not for ordinary members", func(t *testing.T) {
		f.res.AddMember(f.idp.Issuer(), "alice", a, "viewer")
		f.logs.Reset()
		do(srv.authMiddleware(probe(&probeResult{})), "GET", "/v1/traces", f.idp.Sign(t, f.idp.Claims("alice")), nil)
		if strings.Contains(f.logs.String(), "admin selected tenant") {
			t.Error("a member was audited as an admin")
		}
	})
}

func TestAdmin_TenantsEndpoint(t *testing.T) {
	f := newAuthFixture(t)
	a := f.res.AddTenant("Acme", "active")
	b := f.res.AddTenant("Beta", "active")
	f.res.AddTenant("Old", "suspended")
	f.res.AddMember(f.idp.Issuer(), "alice", a, "admin")
	h := f.adminServer(t).Handler()

	t.Run("an admin lists every active tenant, with no memberships written", func(t *testing.T) {
		rec := do(h, "GET", "/v1/tenants", f.idp.Sign(t, f.idp.Claims("root")), nil)
		if rec.Code != 200 {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
		}
		var got []map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body must be a bare array: %v (%s)", err, rec.Body)
		}
		if len(got) != 2 || got[0]["id"] != a.String() || got[1]["id"] != b.String() || got[0]["role"] != "viewer" {
			t.Errorf("got %v", got)
		}
		if ms, _ := f.res.Memberships(context.Background(), f.idp.Issuer(), "root"); len(ms) != 0 {
			t.Errorf("admin access wrote memberships: %v", ms)
		}
	})

	t.Run("a non-admin still sees only their memberships", func(t *testing.T) {
		rec := do(h, "GET", "/v1/tenants", f.idp.Sign(t, f.idp.Claims("alice")), nil)
		var got []map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0]["id"] != a.String() || got[0]["role"] != "admin" {
			t.Fatalf("got %v err %v", got, err)
		}
	})

	t.Run("admin off: the same subject sees nothing", func(t *testing.T) {
		plain := f.server(f.authConfig(false, nil), config.CORSConfig{}).Handler()
		rec := do(plain, "GET", "/v1/tenants", f.idp.Sign(t, f.idp.Claims("root")), nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("status=%d body=%q", rec.Code, rec.Body)
		}
	})
}
