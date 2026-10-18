package metricsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/oidc/oidctest"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

func strp(s string) *string { return &s }

func signTestJWT(t *testing.T, secret string, tenantID uuid.UUID) string {
	t.Helper()
	claims := jwt.MapClaims{
		"tenant_id": tenantID.String(),
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return s
}

func TestMetricsAPI_EndToEnd_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live metrics API test")
	}
	ctx := context.Background()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	tenantID := uuid.New()
	otherTenantID := uuid.New()
	for _, id := range []uuid.UUID{tenantID, otherTenantID} {
		if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, id, id.String(), "t"); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO tenant_config (tenant_id, tier_ceiling, tier_enforcement_mode) VALUES ($1, 'basic', 'strip')`, id); err != nil {
			t.Fatalf("seed tenant_config: %v", err)
		}
	}

	w := storage.NewWriter(pool)
	now := time.Now()
	var rows []storage.TraceRow
	for i := 0; i < 20; i++ {
		exitCode := int32(0)
		if i%4 == 0 {
			exitCode = 1
		}
		rows = append(rows, storage.TraceRow{
			Time: now.Add(-time.Duration(i) * time.Minute), TenantID: tenantID,
			TraceID: uuid.NewString(), SpanID: uuid.NewString(), SpanName: "cli.command.deploy",
			StartTime: now, EndTime: now.Add(100 * time.Millisecond), DurationMs: 100,
			StatusCode: 1, CommandName: strp("deploy"), ExitCode: &exitCode, ConsentTier: "anonymous",
		})
	}
	if _, err := w.InsertTraces(ctx, rows); err != nil {
		t.Fatalf("insert traces: %v", err)
	}
	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('cagg_command_stats_hourly', NULL, NULL)`); err != nil {
		t.Fatalf("refresh hourly cagg: %v", err)
	}
	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('cagg_command_stats_daily', NULL, NULL)`); err != nil {
		t.Fatalf("refresh daily cagg: %v", err)
	}

	secret := "test-signing-secret"
	resolver := tenant.NewCache(tenant.NewStore(pool), time.Minute)
	srv := NewServer(config.MetricsConfig{
		MaxResultPageSize:     1000,
		DefaultResultPageSize: 100,
		MaxTimeRangeSpan:      90 * 24 * time.Hour,
		QueryTimeout:          5 * time.Second,
	}, config.AuthConfig{
		Methods:       []config.AuthMethod{config.AuthMethodJWT},
		JWTSigningKey: secret,
	}, storage.NewQueryBuilder(pool), resolver, "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	token := signTestJWT(t, secret, tenantID)
	fromParam := now.Add(-24 * time.Hour).Format(time.RFC3339)

	t.Run("no token is rejected", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("%s/v1/traces?from=%s", ts.URL, url.QueryEscape(fromParam)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("valid token returns only this tenant's traces", func(t *testing.T) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/traces?from=%s&limit=100", ts.URL, url.QueryEscape(fromParam)), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body struct {
			Data []storage.TraceSummary `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Data) != 20 {
			t.Fatalf("got %d traces, want 20", len(body.Data))
		}
	})

	t.Run("other tenant's token cannot see this tenant's data", func(t *testing.T) {
		otherToken := signTestJWT(t, secret, otherTenantID)
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/traces?from=%s", ts.URL, url.QueryEscape(fromParam)), nil)
		req.Header.Set("Authorization", "Bearer "+otherToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Data []storage.TraceSummary `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		if len(body.Data) != 0 {
			t.Fatalf("other tenant's token saw %d rows, want 0", len(body.Data))
		}
	})

	t.Run("error rate reflects known 25%% error rate", func(t *testing.T) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/metrics/error-rate?from=%s&bucket=day", ts.URL, url.QueryEscape(fromParam)), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body struct {
			Data []storage.ErrorRatePoint `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		var totalInv, totalErr int64
		for _, p := range body.Data {
			totalInv += p.InvocationCount
			totalErr += p.ErrorCount
		}
		if totalInv != 20 || totalErr != 5 {
			t.Fatalf("invocations=%d errors=%d, want 20/5", totalInv, totalErr)
		}
	})

	get := func(t *testing.T, path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("multi-value command filter ORs values", func(t *testing.T) {
		resp := get(t, fmt.Sprintf("/v1/traces?from=%s&command=deploy,nonexistent&command=other", url.QueryEscape(fromParam)))
		defer resp.Body.Close()
		var body struct {
			Data []storage.TraceSummary `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		if len(body.Data) != 20 {
			t.Fatalf("got %d traces, want 20", len(body.Data))
		}
	})

	t.Run("bucket values are validated per endpoint", func(t *testing.T) {
		cases := []struct {
			path   string
			status int
		}{
			{"/v1/metrics/command-frequency?bucket=week", http.StatusOK},
			{"/v1/metrics/exit-codes?bucket=month", http.StatusOK},
			{"/v1/metrics/ci-split?bucket=hour", http.StatusOK},
			{"/v1/metrics/cohorts?bucket=week", http.StatusOK},
			{"/v1/metrics/active-installs?bucket=month", http.StatusOK},
			{"/v1/metrics/retention?bucket=day", http.StatusOK},
			{"/v1/metrics/error-types?bucket=hour", http.StatusOK},
			{"/v1/metrics/friction?bucket=month", http.StatusOK},
			{"/v1/metrics/friction?bucket=year", http.StatusBadRequest},
			{"/v1/metrics/install-funnel?bucket=month", http.StatusOK},
			{"/v1/metrics/install-activation?bucket=week", http.StatusOK},
			{"/v1/metrics/version-changes?bucket=day", http.StatusOK},
			{"/v1/metrics/install-coverage?bucket=week", http.StatusOK},
			{"/v1/metrics/install-methods?bucket=week", http.StatusOK},
			{"/v1/metrics/new-vs-returning?bucket=month", http.StatusOK},
			{"/v1/metrics/feature-discovery?kind=feature", http.StatusOK},
			{"/v1/metrics/feature-discovery?kind=flag", http.StatusOK},
			{"/v1/metrics/feature-discovery?kind=command", http.StatusBadRequest},
			{"/v1/metrics/install-methods?bucket=hour", http.StatusBadRequest},
			{"/v1/metrics/new-vs-returning?bucket=hour", http.StatusBadRequest},
			{"/v1/metrics/install-funnel?bucket=hour", http.StatusBadRequest},
			{"/v1/metrics/install-coverage?bucket=hour", http.StatusBadRequest},
			{"/v1/metrics/cohorts?bucket=hour", http.StatusBadRequest},
			{"/v1/metrics/retention?bucket=hour", http.StatusBadRequest},
			{"/v1/metrics/latency?bucket=year", http.StatusBadRequest},
		}
		for _, c := range cases {
			resp := get(t, c.path+"&from="+url.QueryEscape(fromParam))
			resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Errorf("%s: status = %d, want %d", c.path, resp.StatusCode, c.status)
			}
		}
	})

	t.Run("dimensions endpoint needs no time range", func(t *testing.T) {
		resp := get(t, "/v1/meta/dimensions")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body struct {
			Data storage.Dimensions `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		if !slices.Equal(body.Data.CommandNames, []string{"deploy"}) {
			t.Fatalf("command_name = %v, want [deploy]", body.Data.CommandNames)
		}
	})

	t.Run("tampered JWT signature is rejected", func(t *testing.T) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/traces?from=%s", ts.URL, url.QueryEscape(fromParam)), nil)
		req.Header.Set("Authorization", "Bearer "+token+"tampered")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 for tampered token", resp.StatusCode)
		}
	})

	t.Run("time range wider than MaxTimeRangeSpan is rejected", func(t *testing.T) {
		tooOld := now.Add(-365 * 24 * time.Hour).Format(time.RFC3339)
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/traces?from=%s", ts.URL, url.QueryEscape(tooOld)), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for oversized time range", resp.StatusCode)
		}
	})
}

// TestMetricsAPI_OIDC_Live runs the whole path against a real Postgres: a
// verified OIDC identity, resolved through tenant_members, scopes every query
// to a tenant it owns.
func TestMetricsAPI_OIDC_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live metrics API OIDC test")
	}
	ctx := context.Background()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	store := tenant.NewStore(pool)

	suffix := uuid.NewString()[:8]
	acme, err := store.CreateTenant(ctx, "oidc-acme-"+suffix, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := store.CreateTenant(ctx, "oidc-beta-"+suffix, "Beta")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := store.CreateTenant(ctx, "oidc-foreign-"+suffix, "Foreign")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	var rows []storage.TraceRow
	for i := 0; i < 7; i++ {
		rows = append(rows, storage.TraceRow{
			Time: now.Add(-time.Duration(i) * time.Minute), TenantID: acme.ID,
			TraceID: uuid.NewString(), SpanID: uuid.NewString(), SpanName: "cli.command.deploy",
			StartTime: now, EndTime: now.Add(time.Millisecond), DurationMs: 1,
			StatusCode: 1, CommandName: strp("deploy"), ConsentTier: "anonymous",
		})
	}
	if _, err := storage.NewWriter(pool).InsertTraces(ctx, rows); err != nil {
		t.Fatalf("insert traces: %v", err)
	}

	idp := oidctest.New(t)
	for _, m := range []struct {
		sub string
		t   uuid.UUID
	}{{"solo", acme.ID}, {"both", acme.ID}, {"both", beta.ID}} {
		if _, _, err := store.AddMember(ctx, m.t, idp.Issuer(), m.sub, ""); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	idp.AddOpaque("opaque-both", "both", nil)

	srv := NewServer(config.MetricsConfig{
		MaxResultPageSize: 1000, DefaultResultPageSize: 100,
		MaxTimeRangeSpan: 90 * 24 * time.Hour, QueryTimeout: 5 * time.Second,
	}, config.AuthConfig{
		Methods: []config.AuthMethod{config.AuthMethodOIDC},
		OIDC: config.OIDCConfig{
			IssuerURL: idp.Issuer(), Audience: oidctest.Audience, JWKSCacheTTL: time.Hour,
			RequiredScopes: []string{"metrics:read"},
			Introspection: config.OIDCIntrospectionConfig{
				Enabled: true, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
				CacheTTL: time.Minute, NegativeCacheTTL: time.Second,
			},
		},
	}, storage.NewQueryBuilder(pool), tenant.NewCache(store, time.Minute), "",
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	from := url.QueryEscape(now.Add(-24 * time.Hour).Format(time.RFC3339))
	call := func(t *testing.T, path, token, tenantHeader string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if tenantHeader != "" {
			req.Header.Set(TenantHeader, tenantHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	traceCount := func(t *testing.T, token, tenantHeader string) int {
		t.Helper()
		status, body := call(t, "/v1/traces?from="+from, token, tenantHeader)
		if status != http.StatusOK {
			t.Fatalf("status = %d, body %s", status, body)
		}
		var out struct {
			Data []storage.TraceSummary `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return len(out.Data)
	}

	solo := idp.Sign(t, idp.Claims("solo"))
	both := idp.Sign(t, idp.Claims("both"))

	t.Run("one membership: JWT needs no header and sees only that tenant", func(t *testing.T) {
		if n := traceCount(t, solo, ""); n != 7 {
			t.Fatalf("traces = %d, want 7", n)
		}
	})
	t.Run("several memberships: the header chooses, and the other tenant's data stays out", func(t *testing.T) {
		if status, body := call(t, "/v1/traces?from="+from, both, ""); status != http.StatusBadRequest || !strings.Contains(string(body), "tenant_required") {
			t.Fatalf("no header: status=%d body=%s", status, body)
		}
		if n := traceCount(t, both, acme.ID.String()); n != 7 {
			t.Errorf("acme traces = %d, want 7", n)
		}
		if n := traceCount(t, both, beta.ID.String()); n != 0 {
			t.Errorf("beta traces = %d, want 0", n)
		}
	})
	t.Run("a tenant the user does not belong to is 403", func(t *testing.T) {
		status, body := call(t, "/v1/traces?from="+from, both, foreign.ID.String())
		if status != http.StatusForbidden || !strings.Contains(string(body), "no_tenant_access") {
			t.Fatalf("status=%d body=%s", status, body)
		}
	})
	t.Run("opaque token goes through introspection to the same result", func(t *testing.T) {
		if n := traceCount(t, "opaque-both", acme.ID.String()); n != 7 {
			t.Fatalf("traces = %d, want 7", n)
		}
	})
	t.Run("GET /v1/tenants lists both memberships", func(t *testing.T) {
		status, body := call(t, "/v1/tenants", both, "")
		var got []struct{ ID, Name, Role string }
		if status != http.StatusOK || json.Unmarshal(body, &got) != nil || len(got) != 2 {
			t.Fatalf("status=%d body=%s", status, body)
		}
	})
}
