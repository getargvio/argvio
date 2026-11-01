package metricsapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

// TestMetricCatalogSync_Live covers PUT /v1/metric-definitions/{service}: a
// metrics_admin key replaces the catalog and can't read; metrics_query keys
// and non-admin users can't write; invalid or mismatched files are 400.
func TestMetricCatalogSync_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live catalog sync test")
	}
	ctx := context.Background()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	store := tenant.NewStore(pool)
	ten, err := store.CreateTenant(ctx, "sync-"+time.Now().Format("150405.000000000"), "Sync")
	if err != nil {
		t.Fatal(err)
	}
	adminKey, _, err := store.CreateAPIKey(ctx, ten.ID, tenant.ScopeMetricsAdmin)
	if err != nil {
		t.Fatal(err)
	}
	queryKey, _, err := store.CreateAPIKey(ctx, ten.ID, tenant.ScopeMetricsQuery)
	if err != nil {
		t.Fatal(err)
	}
	ingestKey, _, err := store.CreateAPIKey(ctx, ten.ID, tenant.ScopePublicIngest)
	if err != nil {
		t.Fatal(err)
	}

	secret := "sync-secret"
	srv := NewServer(config.MetricsConfig{
		MaxResultPageSize: 1000, DefaultResultPageSize: 100,
		MaxTimeRangeSpan: 90 * 24 * time.Hour, QueryTimeout: 5 * time.Second,
	}, config.AuthConfig{
		Methods:       []config.AuthMethod{config.AuthMethodAPIKey, config.AuthMethodJWT},
		JWTSigningKey: secret,
	}, storage.NewQueryBuilder(pool), tenant.NewCache(store, time.Minute), testTiers, "",
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	srv.Catalog = storage.NewWriter(pool)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	do := func(t *testing.T, method, path, token, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}
	file := `version: 1
service: vulnapi
metrics:
  - {name: vulns.found, kind: counter, title: Vulnerabilities found, dims: [severity]}
  - {name: scan.run, kind: duration, title: Scan time}
`

	t.Run("a metrics_admin key replaces the catalog", func(t *testing.T) {
		code, body := do(t, "PUT", "/v1/metric-definitions/vulnapi", adminKey, file)
		if code != http.StatusOK {
			t.Fatalf("status = %d %s", code, body)
		}
		var out struct {
			Data storage.ApplyResult `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Data.Created) != 2 {
			t.Errorf("result = %s, want 2 created", body)
		}
		code, body = do(t, "PUT", "/v1/metric-definitions/vulnapi", adminKey, file)
		if code != http.StatusOK || !strings.Contains(body, `"unchanged":["vulns.found","scan.run"]`) {
			t.Errorf("same file again = %d %s, want both unchanged", code, body)
		}
		// JSON is YAML too.
		code, body = do(t, "PUT", "/v1/metric-definitions/vulnapi", adminKey,
			`{"version":1,"service":"vulnapi","metrics":[{"name":"vulns.found","kind":"counter","title":"Vulns"}]}`)
		if code != http.StatusOK || !strings.Contains(body, `"deprecated":["scan.run"]`) || !strings.Contains(body, `"updated":["vulns.found"]`) {
			t.Errorf("json file = %d %s, want vulns.found updated and scan.run deprecated", code, body)
		}

		var by string
		if err := pool.QueryRow(ctx, `SELECT updated_by FROM metric_definitions WHERE tenant_id = $1 AND name = 'vulns.found'`, ten.ID).Scan(&by); err != nil || !strings.HasPrefix(by, "api_key:argv_") {
			t.Errorf("updated_by = %q (err %v), want the key prefix", by, err)
		}
	})

	t.Run("a metrics_admin key can't read", func(t *testing.T) {
		for _, path := range []string{"/v1/metric-definitions", "/v1/metrics/counters?from=2026-01-01T00:00:00Z"} {
			if code, _ := do(t, "GET", path, adminKey, ""); code != http.StatusForbidden {
				t.Errorf("GET %s with metrics_admin = %d, want 403", path, code)
			}
		}
		if code, _ := do(t, "GET", "/v1/metric-definitions", queryKey, ""); code != http.StatusOK {
			t.Errorf("GET with metrics_query = %d, want 200", code)
		}
	})

	t.Run("other credentials can't write", func(t *testing.T) {
		if code, _ := do(t, "PUT", "/v1/metric-definitions/vulnapi", queryKey, file); code != http.StatusForbidden {
			t.Errorf("metrics_query key = %d, want 403", code)
		}
		if code, _ := do(t, "PUT", "/v1/metric-definitions/vulnapi", signTestJWT(t, secret, ten.ID), file); code != http.StatusForbidden {
			t.Errorf("jwt user without the admin role = %d, want 403", code)
		}
		if code, _ := do(t, "PUT", "/v1/metric-definitions/vulnapi", ingestKey, file); code != http.StatusUnauthorized {
			t.Errorf("ingest key = %d, want 401", code)
		}
	})

	t.Run("invalid or mismatched files are refused", func(t *testing.T) {
		code, body := do(t, "PUT", "/v1/metric-definitions/vulnapi", adminKey, "version: 1\nservice: vulnapi\nmetrics:\n  - {name: a, kind: gauge, title: A}\n")
		if code != http.StatusBadRequest || !strings.Contains(body, "kind must be") {
			t.Errorf("invalid = %d %s", code, body)
		}
		if code, body := do(t, "PUT", "/v1/metric-definitions/jwtop", adminKey, file); code != http.StatusBadRequest || !strings.Contains(body, "not jwtop") {
			t.Errorf("mismatched service = %d %s", code, body)
		}
		if code, _ := do(t, "PUT", "/v1/metric-definitions/vulnapi", adminKey, strings.Repeat("#", maxCatalogBytes+1)); code != http.StatusRequestEntityTooLarge {
			t.Errorf("oversized = %d, want 413", code)
		}
	})
}
