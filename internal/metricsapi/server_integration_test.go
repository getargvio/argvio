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
	"github.com/getargvio/argvio/internal/errfp"
	"github.com/getargvio/argvio/internal/oidc/oidctest"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

func strp(s string) *string { return &s }

// testTiers is the ranking schema/allowlist/v1.yaml defines.
var testTiers = storage.TierRanks{"anonymous": 0, "basic": 1, "full": 2, "optin_plus": 3}

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
		// Half the runs report resource usage; the rest are from a platform
		// that cannot measure it.
		var rss, cpu *int64
		if i < 10 {
			r, c := int64(i+1)*1_000_000, int64(i+1)
			rss, cpu = &r, &c
		}
		rows = append(rows, storage.TraceRow{
			PeakRSSBytes: rss, CPUTimeMs: cpu,
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
	for _, cagg := range []string{"cagg_command_resources_hourly", "cagg_command_resources_daily"} {
		if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	// Sessions for the version footprint: 1.10.0 reports size and startup,
	// 1.9.0 only startup, and one anonymous-tier session reports nothing.
	var sessions []storage.LogRow
	addSession := func(i int, version string, size, startup *int64) {
		sessions = append(sessions, storage.LogRow{
			Time: now.Add(-time.Duration(i) * time.Minute), TenantID: tenantID, EventName: "cli.session.started",
			CLIVersion: strp(version), OS: strp("linux"), Arch: strp("amd64"), ConsentTier: "basic",
			BinarySizeBytes: size, StartupMs: startup, LogAttributes: storage.Attrs{"cli.install_id": fmt.Sprintf("inst-%d", i)},
		})
	}
	i64 := func(n int64) *int64 { return &n }
	addSession(1, "1.10.0", i64(20_000_000), i64(30))
	addSession(2, "1.10.0", i64(22_000_000), i64(50))
	addSession(3, "1.9.0", nil, i64(80))
	addSession(4, "1.9.0", nil, i64(90))
	sessions = append(sessions, storage.LogRow{
		Time: now, TenantID: tenantID, EventName: "cli.session.started",
		CLIVersion: strp("1.10.0"), OS: strp("linux"), Arch: strp("amd64"), ConsentTier: "anonymous",
	})
	if _, err := w.InsertLogs(ctx, sessions); err != nil {
		t.Fatalf("insert logs: %v", err)
	}
	if _, err := pool.Exec(ctx, `CALL refresh_continuous_aggregate('cagg_session_footprint_daily', NULL, NULL)`); err != nil {
		t.Fatalf("refresh footprint cagg: %v", err)
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
	}, storage.NewQueryBuilder(pool), resolver, testTiers, "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

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

	t.Run("resource usage reports percentiles and the reporting sample size", func(t *testing.T) {
		resp := get(t, "/v1/metrics/resource-usage?bucket=day&from="+url.QueryEscape(fromParam))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body struct {
			Data []storage.ResourceUsagePoint `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var invocations, rssReported, cpuReported int64
		for _, p := range body.Data {
			if p.CommandName != "deploy" {
				t.Errorf("unexpected command %q", p.CommandName)
			}
			invocations += p.InvocationCount
			rssReported += p.PeakRSSBytes.ReportedCount
			cpuReported += p.CPUTimeMs.ReportedCount
			if p.PeakRSSBytes.ReportedCount > 0 && (p.PeakRSSBytes.P50 == nil || p.CPUTimeMs.P99 == nil) {
				t.Errorf("point with reported runs is missing percentiles: %+v", p)
			}
		}
		if invocations != 20 || rssReported != 10 || cpuReported != 10 {
			t.Fatalf("invocations=%d rss reported=%d cpu reported=%d, want 20/10/10", invocations, rssReported, cpuReported)
		}
	})

	t.Run("version footprint is ordered by version and says how many reported", func(t *testing.T) {
		resp := get(t, "/v1/metrics/version-footprint?from="+url.QueryEscape(now.Add(-72*time.Hour).Format(time.RFC3339)))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body struct {
			Data []storage.VersionFootprintPoint `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Data) != 2 {
			t.Fatalf("got %d versions, want 2: %+v", len(body.Data), body.Data)
		}
		old, cur := body.Data[0], body.Data[1]
		if *old.CLIVersion != "1.9.0" || *cur.CLIVersion != "1.10.0" {
			t.Fatalf("versions = %s, %s; want 1.9.0 then 1.10.0 (semver, not lexical)", *old.CLIVersion, *cur.CLIVersion)
		}
		if old.SessionCount != 2 || old.BinarySize.ReportedSessions != 0 || old.BinarySize.Median != nil || old.Startup.ReportedSessions != 2 {
			t.Errorf("1.9.0 = %+v, want 2 sessions, no size, startup from both", old)
		}
		if cur.SessionCount != 3 || cur.BinarySize.ReportedSessions != 2 || cur.BinarySize.ReportedInstalls != 2 || cur.Startup.ReportedSessions != 2 {
			t.Errorf("1.10.0 = %+v, want 3 sessions, 2 reporting size and startup from 2 installs", cur)
		}
		if cur.BinarySize.Min == nil || *cur.BinarySize.Min != 20_000_000 || cur.BinarySize.Max == nil || *cur.BinarySize.Max != 22_000_000 {
			t.Errorf("1.10.0 size min/max = %v/%v, want 20000000/22000000", cur.BinarySize.Min, cur.BinarySize.Max)
		}
	})

	t.Run("version footprint omits figures nobody reported", func(t *testing.T) {
		resp := get(t, "/v1/metrics/version-footprint?cli_version=1.9.0&from="+url.QueryEscape(now.Add(-72*time.Hour).Format(time.RFC3339)))
		defer resp.Body.Close()
		var raw struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(raw.Data) != 1 {
			t.Fatalf("got %d versions, want 1", len(raw.Data))
		}
		size, _ := raw.Data[0]["binary_size_bytes"].(map[string]any)
		if size["reported_sessions"] != float64(0) {
			t.Errorf("reported_sessions = %v, want 0", size["reported_sessions"])
		}
		for _, k := range []string{"median", "min", "max"} {
			if _, present := size[k]; present {
				t.Errorf("binary_size_bytes.%s should be absent when nothing reported", k)
			}
		}
	})

	t.Run("resource usage rejects an unsupported bucket", func(t *testing.T) {
		resp := get(t, "/v1/metrics/resource-usage?bucket=minute&from="+url.QueryEscape(fromParam))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

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

	t.Run("error fingerprints are tenant scoped and honor the current tier ceiling", func(t *testing.T) {
		setCeiling := func(id uuid.UUID, tier string) {
			t.Helper()
			if _, err := pool.Exec(ctx, `UPDATE tenant_config SET tier_ceiling = $2 WHERE tenant_id = $1`, id, tier); err != nil {
				t.Fatalf("set ceiling: %v", err)
			}
		}
		setCeiling(tenantID, "full")
		setCeiling(otherTenantID, "full")
		defer setCeiling(tenantID, "basic")
		defer setCeiling(otherTenantID, "basic")

		msg := "open /home/alice/.toolrc: permission denied"
		fp, _ := errfp.Compute(msg, "")
		var errs []storage.LogRow
		for i, id := range []uuid.UUID{tenantID, tenantID, otherTenantID} {
			errs = append(errs, storage.LogRow{
				Time: now.Add(-time.Duration(i) * time.Minute), TenantID: id, EventName: errfp.EventName,
				CommandName: strp("deploy"), CLIVersion: strp("1.10.0"), ConsentTier: "full", ErrorFingerprint: &fp.Fingerprint,
				LogAttributes: storage.Attrs{"cli.install_id": fmt.Sprintf("inst-%d", i), errfp.AttrMessage: msg},
			})
		}
		if _, err := w.InsertLogs(ctx, errs); err != nil {
			t.Fatalf("insert error logs: %v", err)
		}

		fetch := func(t *testing.T) []storage.ErrorFingerprint {
			t.Helper()
			resp := get(t, "/v1/metrics/error-fingerprints?from="+url.QueryEscape(fromParam))
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var body struct {
				Data []storage.ErrorFingerprint `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			return body.Data
		}

		got := fetch(t)
		if len(got) != 1 || got[0].Fingerprint != fp.Fingerprint || got[0].EventCount != 2 || got[0].AffectedInstalls != 2 {
			t.Fatalf("got %+v, want this tenant's 2 events under %s", got, fp.Fingerprint)
		}
		if got[0].Message == nil || *got[0].Message != "open <path>: permission denied" {
			t.Errorf("message = %v, want the normalized sample", got[0].Message)
		}

		// Lowering the ceiling takes effect at once: it's read per query, not
		// from the API-key cache.
		setCeiling(tenantID, "basic")
		if got := fetch(t); len(got) != 0 {
			t.Errorf("after lowering the ceiling to basic: got %+v, want none", got)
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
	}, storage.NewQueryBuilder(pool), tenant.NewCache(store, time.Minute), testTiers, "",
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
