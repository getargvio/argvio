package metricsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/storage"
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
	srv := NewServer(config.MetricsConfig{
		AuthMode:              config.AuthModeJWT,
		JWTSigningKey:         secret,
		MaxResultPageSize:     1000,
		DefaultResultPageSize: 100,
		MaxTimeRangeSpan:      90 * 24 * time.Hour,
		QueryTimeout:          5 * time.Second,
	}, storage.NewQueryBuilder(pool), nil, "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

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
